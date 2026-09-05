package hostapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	Layers    []string
}

var (
	// Debug enables more verbose logging
	Debug bool = false
	// Verbose enables verbose logging
	Verbose bool = false
)

// shortLinkFor returns the overlay2/l/<short> symlink path for a layer
// directory, reading the layer's own link file.
func shortLinkFor(overlay2Dir, layerDir string) (string, error) {
	linkBytes, err := os.ReadFile(filepath.Join(layerDir, "link"))
	if err != nil {
		return "", fmt.Errorf("reading link file for %s: %w", layerDir, err)
	}
	return filepath.Join(overlay2Dir, "l", strings.TrimSpace(string(linkBytes))), nil
}

// buildLowerDirs returns the readonly overlay lowerdir list for a top layer:
// the top layer first, then its parent chain, every entry referenced by the
// engine's compact overlay2/l/<short> symlink. Init layers are dropped.
func buildLowerDirs(overlay2Dir, layerDir string) ([]string, error) {
	top, err := shortLinkFor(overlay2Dir, layerDir)
	if err != nil {
		return nil, err
	}
	lowerDirs := []string{top}

	lowerBytes, err := os.ReadFile(filepath.Join(layerDir, "lower"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading lower file: %w", err)
	}
	if len(lowerBytes) == 0 {
		return lowerDirs, nil
	}

	for _, link := range strings.Split(strings.TrimSpace(string(lowerBytes)), ":") {
		linkPath := filepath.Join(overlay2Dir, link)
		target, err := os.Readlink(linkPath)
		if err != nil {
			return nil, fmt.Errorf("resolving %s: %w", link, err)
		}
		if strings.Contains(target, "-init/") {
			if Debug {
				log.Printf("Skipping init layer: %s", target)
			}
			continue
		}
		lowerDirs = append(lowerDirs, linkPath)
	}
	return lowerDirs, nil
}

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

	lowerDirs, err := buildLowerDirs(overlay2Dir, layerDir)
	if err != nil {
		return "", err
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
	container.Layers = lowerDirs
	log.Printf("Mounted ID %s in %s\n", container.ID, container.MountPath)

	return container.MountPath, nil
}

// Unmount releases the container's overlay filesystem. It is a no-op for a
// container that was never mounted.
func (container *Container) Unmount() error {
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

// readContainers returns the container records under rootdir matching match,
// without mounting anything. Dead and removal-in-progress containers are
// dropped here because they contribute nothing to a boot; every caller must
// see the same set the mount path does, so this exclusion has a single home.
func readContainers(rootdir string, match string) ([]Container, error) {
	containersDir := filepath.Join(rootdir, "containers")
	entries, err := os.ReadDir(containersDir)
	if err != nil {
		return nil, fmt.Errorf("reading containers directory: %w", err)
	}

	var found []Container

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

		found = append(found, container)
	}

	return found, nil
}

// initializeContainers finds and mounts containers
func initializeContainers(rootdir string, match string) ([]Container, error) {
	containers, err := readContainers(rootdir, match)
	if err != nil {
		return nil, err
	}

	var mountedContainers []Container

	for i := range containers {
		if _, err := containers[i].mount(rootdir); err != nil {
			log.Println("Failed to mount container:", err)
			continue
		}
		mountedContainers = append(mountedContainers, containers[i])
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
	HOSTOS_BLOCKS_CLASS          = "io.balena.image.class"
	HOSTOS_BLOCKS_OVERRIDE       = "io.balena.image.override"
	HOSTOS_BLOCKS_KERNEL_VERSION = "io.balena.image.kernel-version"
	HOSTOS_BLOCKS_KERNEL_ABI_ID  = "io.balena.image.kernel-abi-id"
	CMDLINE_KERNEL_ABI           = "balena_kernel_abi"
	PURGE_MARKER_FILE            = "remove_me_to_reset"
)

// ErrClaimsUnavailable reports that the deployed kernel ABI claims cannot be
// determined from the store.
var ErrClaimsUnavailable = errors.New("cannot determine the deployed kernel ABI claims")

// ClaimedKernelABIs returns the kernel ABI ids mobynit will mount module trees
// for on this boot, read straight from the container store with no engine
// running.
//
// Four states, which callers read differently:
//
//	data root absent                          the stat error
//	purge armed, no remove_me_to_reset        ErrClaimsUnavailable
//	containers directory missing              ErrClaimsUnavailable
//	containers present, none claiming an ABI  nil, nil
//
// A nil slice with a nil error therefore means the claim set is genuinely
// empty and nothing else. The boot path reads ErrClaimsUnavailable as
// "claim nothing"; a caller sweeping records against the claim set reads it
// as "do not act on state you cannot read".
func ClaimedKernelABIs(rootdir string) ([]string, error) {
	// An absent data root means the partition is not mounted or the path is
	// wrong.
	if _, err := os.Stat(rootdir); err != nil {
		return nil, err
	}

	// A purge boot wipes the data partition, and mountDataOverlays skips every
	// extension overlay because of it. Answering with the deployed claims would
	// let the caller select a kernel whose modules this boot leaves unmounted.
	// The store is <data>/docker, so the marker is one level up. An unreadable
	// marker is not proof that the purge is disarmed.
	marker := filepath.Join(filepath.Dir(filepath.Clean(rootdir)), PURGE_MARKER_FILE)
	if _, err := os.Stat(marker); err != nil {
		return nil, ErrClaimsUnavailable
	}

	containers, err := readContainers(rootdir, HOSTOS_BLOCKS_CLASS)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrClaimsUnavailable
		}
		return nil, err
	}

	seen := make(map[string]struct{}, len(containers))
	var abis []string
	for _, c := range containers {
		abi := c.Labels[HOSTOS_BLOCKS_KERNEL_ABI_ID]
		if abi == "" {
			continue
		}
		if _, dup := seen[abi]; dup {
			continue
		}
		seen[abi] = struct{}{}
		abis = append(abis, abi)
	}
	sort.Strings(abis)
	return abis, nil
}

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
// Used to derive io.balena.image.kernel-abi-id from the kernel image.
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

// KernelImageForABIID returns the path of the regular file directly under
// bootDir whose sha256 equals abi, or "" when the directory holds no match.
// Only an unusable bootDir is an error, wrapping os.ErrNotExist when it is
// absent, so callers can tell "nothing matches" from "cannot tell".
//
// Files that cannot be hashed are skipped, and the reasons returned for the
// caller to report with whatever context it has.
func KernelImageForABIID(bootDir, abi string) (string, []error, error) {
	if abi == "" {
		return "", nil, fmt.Errorf("no kernel ABI id to match against under %s", bootDir)
	}
	entries, err := os.ReadDir(bootDir)
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", bootDir, err)
	}
	var skipped []error
	for _, e := range entries {
		// A symlink's bytes could live outside the extension
		if !e.Type().IsRegular() {
			continue
		}
		image := filepath.Join(bootDir, e.Name())
		id, err := ComputeABIID(image)
		if err != nil {
			// An unreadable file must not mask a later match
			skipped = append(skipped, err)
			continue
		}
		if id == abi {
			return image, skipped, nil
		}
	}
	return "", skipped, nil
}

// ResolveExtensionABIID returns the extension's kernel identity claim.
//   - The extension carries a modules tree
//   - A file directly under the extension's /boot (the shipped kernel image)
//     hashes to hostABIID, the balena_kernel_abi cmdline token.
//
// Returns "" (no claim) when:
// * The extension is unmounted
// * Carries no modules tree
// * carries one for another release
//
// Returns errors when an extension claims the running release but cannot be
// matched to the running kernel: unknown release, stock kernel (empty
// hostABIID), no /boot, or no /boot file hashing to hostABIID.
func (c *Container) ResolveExtensionABIID(release, hostABIID string) (string, error) {
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

	if hostABIID == "" {
		return "", fmt.Errorf("extension %s: kernel-carrying but running kernel is stock", c.Name)
	}

	// The kernel-abi-id label is advisory: warn on absence or divergence for
	// observability, but never gate the mount on it.
	if labelVal := c.Labels[HOSTOS_BLOCKS_KERNEL_ABI_ID]; labelVal == "" {
		log.Printf("Warning: extension %s: modules present but no %s label", c.Name, HOSTOS_BLOCKS_KERNEL_ABI_ID)
	} else if labelVal != hostABIID {
		log.Printf("Warning: extension %s: %s label %q != running kernel %q; using image match", c.Name, HOSTOS_BLOCKS_KERNEL_ABI_ID, labelVal, hostABIID)
	}

	bootDir := filepath.Join(c.MountPath, "boot")
	image, skipped, err := KernelImageForABIID(bootDir, hostABIID)
	for _, s := range skipped {
		log.Printf("Warning: extension %s: %v", c.Name, s)
	}
	if err != nil {
		// os.IsNotExist does not unwrap
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("broken extension %s: modules present but no kernel image under /boot", c.Name)
		}
		return "", fmt.Errorf("broken extension %s: %w", c.Name, err)
	}
	if image == "" {
		return "", fmt.Errorf("extension %s: no /boot kernel image matches running kernel %q", c.Name, hostABIID)
	}
	return hostABIID, nil
}

// FilterByKernelABIID keeps only those containers safe to mount over the
// running kernel.
//
// An ABI-agnostic extension makes no kernel-ABI claim and always passes.
// A kernel-carrying extension is kept only when one of its /boot images
// hashes to hostABIID; ResolveExtensionABIID errors otherwise, so no
// further comparison is needed here.
func FilterByKernelABIID(containers []Container, release, hostABIID string) []Container {
	var filtered []Container
	for i := range containers {
		c := &containers[i]
		if _, err := c.ResolveExtensionABIID(release, hostABIID); err != nil {
			log.Printf("Error: dropping container %s: %v", c.Name, err)
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
		if err := containers[i].Unmount(); err != nil {
			log.Printf("Warning: failed to unmount dropped extension %s: %v", containers[i].Name, err)
		}
	}
	return selected
}

// dedupLayers drops repeated layer references, keeping the LAST occurrence
// of each. Overlayfs rejects a lowerdir naming the same directory twice
// (ELOOP).
func dedupLayers(layers []string) []string {
	seen := make(map[string]struct{}, len(layers))
	out := make([]string, 0, len(layers))
	for i := len(layers) - 1; i >= 0; i-- {
		if _, dup := seen[layers[i]]; dup {
			if Debug {
				log.Printf("Dropping duplicate shared layer %s", layers[i])
			}
			continue
		}
		seen[layers[i]] = struct{}{}
		out = append(out, layers[i])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Extension represents an OS-block overlay extension as a layer chain.
// Extensions passed in the leftExtensions slice of BuildOverlayOptions layer
// left of the base chain in lowerdir at their Priority (lower = higher
// overlayfs precedence, ties broken by Name). Extensions passed in
// rightExtensions layer right of the base chain; their Priority field is
// ignored.
type Extension struct {
	Name     string
	Layers   []string
	Priority int
}

// BuildOverlayOptions constructs a flat overlay lowerdir mount options string
// from per-image layer chains.
//
// Extensions whose chain would push the options string past the kernel
// page-size limit are dropped as WHOLE chains (a partial chain would compose
// a partial image): rightExtensions first, then the lowest-priority
// leftExtensions. Drops are logged per name. The set that fits is logged in
// mount order.
func BuildOverlayOptions(baseLayers []string, leftExtensions, rightExtensions []Extension) string {
	sort.Slice(leftExtensions, func(i, j int) bool {
		if leftExtensions[i].Priority != leftExtensions[j].Priority {
			return leftExtensions[i].Priority < leftExtensions[j].Priority
		}
		return leftExtensions[i].Name < leftExtensions[j].Name
	})

	pageLimit := os.Getpagesize() - 1
	base := strings.Join(baseLayers, ":")

	prefix := "lowerdir="
	leftIncluded := 0
	for _, e := range leftExtensions {
		chain := strings.Join(e.Layers, ":")
		candidate := prefix + chain + ":" + base
		if len(candidate) >= pageLimit {
			break
		}
		prefix += chain + ":"
		leftIncluded++
	}
	for _, e := range leftExtensions[leftIncluded:] {
		log.Printf("Warning: extension %q dropped due to page size limit", e.Name)
	}

	opts := prefix + base

	// Phase 2: append rightExtensions as space allows
	rightIncluded := 0
	for _, e := range rightExtensions {
		candidate := opts + ":" + strings.Join(e.Layers, ":")
		if len(candidate) >= pageLimit {
			break
		}
		opts = candidate
		rightIncluded++
	}
	for _, e := range rightExtensions[rightIncluded:] {
		log.Printf("Warning: extension %q dropped due to page size limit", e.Name)
	}

	// Dedup shared layers AFTER the page-budget drops
	opts = "lowerdir=" + strings.Join(dedupLayers(strings.Split(strings.TrimPrefix(opts, "lowerdir="), ":")), ":")

	// Log what fit, in mount order
	log.Println("Overlayed images:")
	idx := 0
	for i := 0; i < leftIncluded; i++ {
		e := leftExtensions[i]
		log.Printf("\t[%d] %s (left, priority=%d, %d layers)", idx, e.Name, e.Priority, len(e.Layers))
		idx++
	}
	log.Printf("\t[%d] hostapp (%d layers)", idx, len(baseLayers))
	idx++
	for i := 0; i < rightIncluded; i++ {
		e := rightExtensions[i]
		log.Printf("\t[%d] %s (right, %d layers)", idx, e.Name, len(e.Layers))
		idx++
	}

	return opts
}
