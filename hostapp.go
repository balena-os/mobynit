package hostapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	HomePath  string
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

// unmount releases the container's overlay filesystem. It is a no-op for a
// container that was never mounted (MountPath == "").
func (container *Container) unmount() error {
	if container.MountPath == "" {
		return nil
	}
	if err := unix.Unmount(container.MountPath, 0); err != nil {
		return fmt.Errorf("unmounting %s: %w", container.MountPath, err)
	}
	if Debug {
		log.Printf("Unmounted ID %s from %s", container.ID, container.MountPath)
	}
	container.MountPath = ""
	return nil
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
	container.HomePath = homePath
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

const (
	HOSTOS_BLOCKS_OVERRIDE       = "io.balena.image.override"
	HOSTOS_BLOCKS_KERNEL_VERSION = "io.balena.image.kernel-version"
	HOSTOS_BLOCKS_KERNEL_ABI_ID  = "io.balena.image.kernel-abi-id"
	CMDLINE_KERNEL_ABI           = "balena_kernel_abi"
)

// ParseHostKernelABIID extracts the balena_kernel_abi=<value> token from a
// kernel cmdline string and returns its value. Returns "" when the token is
// absent or carries an empty value, i.e. when the boot path ran a stock
// kernel whose ABI is not knowable.
func ParseHostKernelABIID(cmdline string) string {
	prefix := CMDLINE_KERNEL_ABI + "="
	for _, tok := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(tok, prefix); ok {
			return v
		}
	}
	return ""
}

// GetKernelRelease returns the running kernel's full release string
// (e.g. "6.8.0-100-generic"), as reported by uname(2).
func GetKernelRelease() (string, error) {
	var utsname unix.Utsname
	if err := unix.Uname(&utsname); err != nil {
		return "", fmt.Errorf("uname syscall failed: %w", err)
	}
	return unix.ByteSliceToString(utsname.Release[:]), nil
}

// kernelVersionFromRelease strips the local-version suffix (e.g. "-100-generic",
// "-v8+") from a uname release, leaving the M.m.p version that kernel-version
// compatibility tracks. An empty release yields an empty version.
func kernelVersionFromRelease(release string) string {
	if idx := strings.IndexByte(release, '-'); idx > 0 {
		return release[:idx]
	}
	return release
}

// FilterByKernelVersion removes containers whose kernel-version label
// doesn't match the running kernel. Containers without the label always pass.
// An empty kernelVersion disables filtering.
func FilterByKernelVersion(containers []Container, kernelVersion string) []Container {
	if kernelVersion == "" {
		return containers
	}
	var filtered []Container
	for _, c := range containers {
		if labelVal, ok := c.Labels[HOSTOS_BLOCKS_KERNEL_VERSION]; ok && labelVal != kernelVersion {
			log.Printf("Skipping container %s: kernel version %q != running %q", c.Name, labelVal, kernelVersion)
			continue
		}
		filtered = append(filtered, c)
	}
	return filtered
}

// ComputeABIID returns the hex-encoded sha256 of the file at path.
// Used to derive io.balena.image.kernel-abi-id from Module.symvers.
func ComputeABIID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ResolveExtensionABIID computes the container's kernel ABI ID as
// sha256(<mount>/lib/modules/<release>/Module.symvers), where release is the
// running kernel's uname release.
//
// Returns "" with no error if the extension carries no kernel modules for the
// running release. Returns an error if /lib/modules/<release> exists
// but Module.symvers is missing (broken extension), if the label disagrees with
// the computed value, or if a module-carrying extension cannot be verified
// because release is empty (running kernel unknown).
func (c *Container) ResolveExtensionABIID(release string) (string, error) {
	if c.MountPath == "" {
		return "", nil
	}

	modulesRoot := filepath.Join(c.MountPath, "lib", "modules")
	if _, err := os.Stat(modulesRoot); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat %s: %w", modulesRoot, err)
	}

	if release == "" {
		return "", fmt.Errorf("extension %s: running kernel release unknown", c.Name)
	}
	modDir := filepath.Join(modulesRoot, release)
	if _, err := os.Stat(modDir); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat %s: %w", modDir, err)
	}
	symversPath := filepath.Join(modDir, "Module.symvers")
	if _, err := os.Stat(symversPath); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("broken extension %s: %s missing", c.Name, symversPath)
		}
		return "", fmt.Errorf("stat %s: %w", symversPath, err)
	}
	id, err := ComputeABIID(symversPath)
	if err != nil {
		return "", err
	}
	if labelVal, ok := c.Labels[HOSTOS_BLOCKS_KERNEL_ABI_ID]; ok && labelVal != "" && labelVal != id {
		return "", fmt.Errorf("extension %s: %s label %q != computed %q",
			c.Name, HOSTOS_BLOCKS_KERNEL_ABI_ID, labelVal, id)
	}
	return id, nil
}

// FilterByKernelABIID keeps only those containers safe to mount over the
// running kernel.
//
// An ABI-agnostic extension makes no kernel-ABI claim and always passes.
// A kernel-carrying extension is kept only when its computed ABI equals hostABIID.
func FilterByKernelABIID(containers []Container, release, hostABIID string) []Container {
	var filtered []Container
	for i := range containers {
		c := &containers[i]
		id, err := c.ResolveExtensionABIID(release)
		if err != nil {
			log.Printf("Error: dropping container %s: %v", c.Name, err)
			continue
		}
		if id == "" {
			filtered = append(filtered, *c)
			continue
		}
		if id != hostABIID {
			log.Printf("Skipping container %s: kernel ABI ID %q != host %q", c.Name, id, hostABIID)
			continue
		}
		filtered = append(filtered, *c)
	}
	return filtered
}

// SelectMountable filters the already-mounted extensions down to those
// compatible with the running kernel, unmounting every extension it drops.
// Survivors stay mounted for use as overlay lowerdirs.
func SelectMountable(containers []Container, release, hostABIID string) []Container {
	selected := FilterByKernelVersion(containers, kernelVersionFromRelease(release))
	selected = FilterByKernelABIID(selected, release, hostABIID)

	keep := make(map[string]bool, len(selected))
	for _, c := range selected {
		keep[c.MountPath] = true
	}
	for i := range containers {
		if keep[containers[i].MountPath] {
			continue
		}
		if err := containers[i].unmount(); err != nil {
			log.Printf("Warning: failed to unmount dropped extension %s: %v", containers[i].Name, err)
		}
	}
	return selected
}

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
