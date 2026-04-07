package hostapp

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

type HostConfig struct {
	Labels map[string]string `json:"Labels"`
}

type State struct {
	Dead              bool `json:"Dead"`
	RemovalInProgress bool `json:"RemovalInProgress"`
}

type Config struct {
	HostConfig `json:"Config"`

	ID     string `json:"ID"`
	Image  string `json:"Image"`
	Name   string `json:"Name"`
	Driver string `json:"Driver"`
	State  State  `json:"State"`
}

type Container struct {
	Config
	MountPath string
}

var (
	// Debug enables more verbose logging
	Debug bool = false
	// Verbose enables verbose logging
	Verbose bool = false
)

// mount mounts the container's overlay filesystem using direct overlay2 metadata reading
func (container *Container) mount(layerRoot string) (string, error) {
	if container.Driver != "overlay2" {
		return "", fmt.Errorf("unsupported driver %s for container %s", container.Driver, container.Name)
	}

	// Get mount-id from layerdb
	mountIDPath := filepath.Join(layerRoot, "image", "overlay2", "layerdb", "mounts", container.ID, "mount-id")
	mountIDBytes, err := os.ReadFile(mountIDPath)
	if err != nil {
		return "", fmt.Errorf("reading mount-id: %w", err)
	}
	mountID := strings.TrimSpace(string(mountIDBytes))

	overlay2Dir := filepath.Join(layerRoot, "overlay2")
	layerDir := filepath.Join(overlay2Dir, mountID)

	// The layer's own diff directory - this is the top layer
	diffDir := filepath.Join(layerDir, "diff")

	// Read lower file to get parent layer chain
	lowerPath := filepath.Join(layerDir, "lower")
	lowerBytes, err := os.ReadFile(lowerPath)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("reading lower file: %w", err)
	}

	// Build lowerdir list: diff first, then all parent layers (including init)
	// For readonly overlay, diff is part of lowerdir (no upperdir)
	lowerDirs := []string{diffDir}

	if len(lowerBytes) > 0 {
		links := strings.Split(strings.TrimSpace(string(lowerBytes)), ":")
		for _, link := range links {
			resolved, err := filepath.EvalSymlinks(filepath.Join(overlay2Dir, link))
			if err != nil {
				return "", fmt.Errorf("resolving %s: %w", link, err)
			}
			// Skip init layers - they contain .dockerenv which causes
			// systemd to detect container mode
			if strings.Contains(resolved, "-init/") {
				if Debug {
					log.Printf("Skipping init layer: %s", resolved)
				}
				continue
			}
			lowerDirs = append(lowerDirs, resolved)
		}
	}

	// Mount point: overlay2/<mount-id>/merged
	mountPoint := filepath.Join(layerDir, "merged")
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return "", fmt.Errorf("creating mount point: %w", err)
	}

	// Build overlay options (readonly - no upperdir/workdir)
	opts := "lowerdir=" + strings.Join(lowerDirs, ":")
	if len(opts) >= os.Getpagesize()-1 {
		return "", fmt.Errorf("mount options (%d bytes) exceed page size limit", len(opts))
	}

	if err := unix.Mount("overlay", mountPoint, "overlay", 0, opts); err != nil {
		return "", fmt.Errorf("mounting overlay: %w", err)
	}

	container.MountPath = mountPoint
	log.Printf("Mounted ID %s in %s\n", container.ID, container.MountPath)

	return container.MountPath, nil
}

// initialize reads container config
func (container *Container) initialize(homePath string) error {
	configPath := filepath.Join(homePath, "config.v2.json")
	f, err := os.Open(configPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", configPath, err)
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(&container.Config); err != nil {
		return fmt.Errorf("decoding %s: %w", configPath, err)
	}
	if Verbose || Debug {
		log.Println("Initialized container:", container.Config.Name)
	}
	return nil
}

// initializeContainers finds and mounts containers
func initializeContainers(rootdir string, match string) ([]Container, error) {
	containersDir := filepath.Join(rootdir, "containers")
	entries, err := os.ReadDir(containersDir)
	if err != nil {
		return nil, fmt.Errorf("reading containers directory: %w", err)
	}

	var mountedContainers []Container

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		homePath := filepath.Join(containersDir, entry.Name())
		var container Container

		if err := container.initialize(homePath); err != nil {
			log.Println("Error initializing container:", err)
			continue
		}

		// Skip dead or pending-removal containers
		if container.State.Dead || container.State.RemovalInProgress {
			log.Printf("Skipping dead container: %s (%s)", container.Name, container.ID)
			continue
		}

		// Match by ID prefix or by label
		matched := false
		if strings.HasPrefix(container.ID, match) {
			matched = true
		} else if val, ok := container.Labels[match]; ok && val == "overlay" {
			matched = true
		}

		if !matched {
			continue
		}

		if _, err := container.mount(rootdir); err != nil {
			log.Println("Failed to mount container:", err)
		} else {
			mountedContainers = append(mountedContainers, container)
		}
	}

	return mountedContainers, nil
}

// Mount finds and mounts container overlay filesystems matching by ID or label
func Mount(rootdir string, label string) ([]Container, error) {
	if Debug {
		log.Printf("Searching for container with ID/label %s in root directory %s\n", label, rootdir)
	}
	return initializeContainers(rootdir, label)
}

const HOSTOS_BLOCKS_OVERRIDE = "io.balena.image.override"

// Extension represents an OS-block overlay extension. Extensions passed in
// the leftExtensions slice of BuildOverlayOptions mount left of the hostapp
// in lowerdir at their Priority (lower = higher overlayfs precedence, ties
// broken by Name). Extensions passed in rightExtensions mount right of the
// hostapp; their Priority field is ignored.
type Extension struct {
	Name      string
	MountPath string
	Priority  int
}

// BuildOverlayOptions constructs an overlay lowerdir mount options string.
// leftExtensions mount left of basePath (taking overlayfs precedence over it)
// sorted by Priority ascending with Name as tie-breaker. rightExtensions mount
// right of basePath in the order given. basePath is always included.
//
// Extensions that would push the options string past the kernel page-size
// limit are dropped: rightExtensions first, then the lowest-priority
// leftExtensions. Drops are logged per name. The set of extensions that fit
// is logged in mount order.
func BuildOverlayOptions(basePath string, leftExtensions, rightExtensions []Extension) string {
	sort.Slice(leftExtensions, func(i, j int) bool {
		if leftExtensions[i].Priority != leftExtensions[j].Priority {
			return leftExtensions[i].Priority < leftExtensions[j].Priority
		}
		return leftExtensions[i].Name < leftExtensions[j].Name
	})

	pageLimit := os.Getpagesize() - 1

	// Phase 1: prepend leftExtensions (highest priority first) while basePath still fits
	prefix := "lowerdir="
	leftIncluded := 0
	for _, e := range leftExtensions {
		candidate := prefix + e.MountPath + ":" + basePath
		if len(candidate) >= pageLimit {
			break
		}
		prefix += e.MountPath + ":"
		leftIncluded++
	}
	for _, e := range leftExtensions[leftIncluded:] {
		log.Printf("Warning: extension %q dropped due to page size limit", e.Name)
	}

	opts := prefix + basePath

	// Phase 2: append rightExtensions as space allows
	rightIncluded := 0
	for _, e := range rightExtensions {
		candidate := opts + ":" + e.MountPath
		if len(candidate) >= pageLimit {
			break
		}
		opts = candidate
		rightIncluded++
	}
	for _, e := range rightExtensions[rightIncluded:] {
		log.Printf("Warning: extension %q dropped due to page size limit", e.Name)
	}

	// Log what fit, in mount order
	log.Println("Overlayed images:")
	idx := 0
	for i := 0; i < leftIncluded; i++ {
		e := leftExtensions[i]
		log.Printf("\t[%d] %s (left, priority=%d)", idx, e.Name, e.Priority)
		idx++
	}
	log.Printf("\t[%d] %s (hostapp)", idx, basePath)
	idx++
	for i := 0; i < rightIncluded; i++ {
		e := rightExtensions[i]
		log.Printf("\t[%d] %s (right)", idx, e.Name)
		idx++
	}

	return opts
}
