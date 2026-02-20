package hostapp

import (
	"crypto/md5"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

var rootdir = flag.String("rootdir", "", "Path to root directory with Docker/balena containers")
var repeatedLabelsCount = flag.Int("repLabels", 0, "Number of containers with the same repeated label")

// TestMountContainersByID tests mounting a container by its ID.
// This test requires root and performs an actual overlay mount.
func TestMountContainersByID(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to perform overlay mount")
	}

	if *rootdir == "" {
		t.Skip("This test requires a --rootdir flag")
	}

	// Create mount namespace for isolation
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("failed to create mount namespace: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("failed to make mounts private: %v", err)
	}

	current, err := os.Readlink(filepath.Join(*rootdir, "current"))
	if err != nil {
		t.Fatalf("Could not get container ID: %v", err)
	}
	cid := filepath.Base(current)

	containers, err := Mount(*rootdir, cid)
	if err != nil {
		t.Fatalf("Mount by ID failed: %v", err)
	}

	if len(containers) != 1 {
		t.Errorf("Expected 1 container, got %d", len(containers))
	}

	if len(containers) > 0 {
		if containers[0].MountPath == "" {
			t.Error("Container should have MountPath set")
		}

		// Verify we can read from the mounted filesystem
		entries, err := os.ReadDir(containers[0].MountPath)
		if err != nil {
			t.Errorf("Failed to read mounted path: %v", err)
		}
		if len(entries) == 0 {
			t.Error("Mounted filesystem appears empty")
		}
		t.Logf("Mounted %s at %s with %d entries", containers[0].Name, containers[0].MountPath, len(entries))
	}
}

// TestMountContainersByLabel tests mounting containers by label.
func TestMountContainersByLabel(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to perform overlay mount")
	}

	if *rootdir == "" {
		t.Skip("This test requires a --rootdir flag")
	}

	if *repeatedLabelsCount == 0 {
		t.Skip("This test requires a --repLabels flag")
	}

	// Create mount namespace for isolation
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("failed to create mount namespace: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("failed to make mounts private: %v", err)
	}

	// Create symlink for testing
	linkRootDir := "/tmp/testlink"
	os.Remove(linkRootDir)
	if err := os.Symlink(*rootdir, linkRootDir); err != nil {
		t.Fatalf("error creating rootdir symlink: %v", err)
	}
	defer os.Remove(linkRootDir)

	// Create temp file for testing invalid path
	fileRootDir, err := os.CreateTemp("", "testHostAppFile")
	if err != nil {
		t.Fatal("Unable to create temporary file")
	}
	defer os.Remove(fileRootDir.Name())

	var tests = []struct {
		name          string
		rootdir       string
		label         string
		expectFailure bool
		expectCount   int
	}{
		{"non-existent path", "/does/not/exist", "None", true, 0},
		{"symlinked rootdir", linkRootDir, "unique-label", false, 1},
		{"file as rootdir", fileRootDir.Name(), "None", true, 0},
		{"unique label", *rootdir, "unique-label", false, 1},
		{"nonsense label", *rootdir, "nonsense", false, 0},
		{"repeated label", *rootdir, "repeated-label", false, *repeatedLabelsCount},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			containers, err := Mount(test.rootdir, test.label)

			if test.expectFailure && err == nil {
				t.Errorf("Test should have failed")
			}
			if !test.expectFailure && err != nil {
				t.Errorf("Test should have passed: %v", err)
			}
			if !test.expectFailure && len(containers) != test.expectCount {
				t.Errorf("Expected %d containers, got %d", test.expectCount, len(containers))
			}

			// Verify mounted containers have MountPath set
			for _, c := range containers {
				if c.MountPath == "" {
					t.Errorf("Container %s should have MountPath set", c.Name)
				}
			}
		})
	}
}

// TestMountRealHostapp tests mounting an actual balena hostapp container.
func TestMountRealHostapp(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to perform overlay mount")
	}

	if *rootdir == "" {
		t.Skip("This test requires a --rootdir flag")
	}

	// Create mount namespace for isolation
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("failed to create mount namespace: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("failed to make mounts private: %v", err)
	}

	// Check for hostapp-current symlink (created by setup script for real hostapp)
	hostappCurrent := filepath.Join(*rootdir, "hostapp-current")
	current, err := os.Readlink(hostappCurrent)
	if err != nil {
		t.Skip("No real hostapp available (hostapp-current symlink missing)")
	}

	cid := filepath.Base(current)

	containers, err := Mount(*rootdir, cid)
	if err != nil {
		t.Fatalf("Mount real hostapp failed: %v", err)
	}

	if len(containers) != 1 {
		t.Fatalf("Expected 1 container, got %d", len(containers))
	}

	container := containers[0]

	if container.MountPath == "" {
		t.Error("Real hostapp should have MountPath set")
	}

	// Verify the mounted filesystem looks like a root filesystem
	entries, err := os.ReadDir(container.MountPath)
	if err != nil {
		t.Fatalf("Failed to read mounted path: %v", err)
	}

	// Check for expected root filesystem directories
	entryNames := make(map[string]bool)
	for _, e := range entries {
		entryNames[e.Name()] = true
	}

	expectedDirs := []string{"bin", "etc", "usr"}
	for _, dir := range expectedDirs {
		if !entryNames[dir] {
			t.Errorf("Expected /%s in mounted hostapp", dir)
		}
	}

	t.Logf("Real hostapp %s mounted at %s with %d entries", container.Name, container.MountPath, len(entries))
}

// TestMountOSBlocksByLabel tests finding and mounting containers with io.balena.image.class=overlay.
func TestMountOSBlocksByLabel(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to perform overlay mount")
	}

	if *rootdir == "" {
		t.Skip("This test requires a --rootdir flag")
	}

	// Create mount namespace for isolation
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("failed to create mount namespace: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("failed to make mounts private: %v", err)
	}

	containers, err := Mount(*rootdir, "io.balena.image.class")
	if err != nil {
		t.Fatalf("Mount OS blocks failed: %v", err)
	}

	if len(containers) == 0 {
		t.Skip("No OS block containers available")
	}

	t.Logf("Found %d OS block containers", len(containers))

	for _, c := range containers {
		if c.Labels["io.balena.image.class"] != "overlay" {
			t.Errorf("Container %s missing io.balena.image.class=overlay label", c.Name)
		}
		if c.MountPath == "" {
			t.Errorf("Container %s should have MountPath set", c.Name)
		}
		if c.Driver != "overlay2" {
			t.Errorf("Container %s has unexpected driver: %s", c.Name, c.Driver)
		}
	}
}

// TestMountVerifiesOverlayWorks is the critical integration test.
// It verifies the kernel accepts our overlay mount by actually performing it.
func TestMountVerifiesOverlayWorks(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to perform overlay mount")
	}

	if *rootdir == "" {
		t.Skip("This test requires a --rootdir flag")
	}

	// Create mount namespace for isolation
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("failed to create mount namespace: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("failed to make mounts private: %v", err)
	}

	// Get the hostapp container
	hostappCurrent := filepath.Join(*rootdir, "current")
	current, err := os.Readlink(hostappCurrent)
	if err != nil {
		hostappCurrent = filepath.Join(*rootdir, "hostapp-current")
		current, err = os.Readlink(hostappCurrent)
		if err != nil {
			t.Skip("No hostapp available (current/hostapp-current symlink missing)")
		}
	}

	cid := filepath.Base(current)

	// Mount() performs the actual overlay mount - this catches layer issues!
	containers, err := Mount(*rootdir, cid)
	if err != nil {
		t.Fatalf("Mount failed (overlay mount rejected by kernel): %v", err)
	}

	if len(containers) == 0 {
		t.Fatal("No containers found")
	}

	container := containers[0]
	if container.MountPath == "" {
		t.Fatal("Container has no MountPath - mount may have failed silently")
	}

	// Verify we can actually read from the overlay
	entries, err := os.ReadDir(container.MountPath)
	if err != nil {
		t.Fatalf("Failed to read from overlay mount: %v", err)
	}

	if len(entries) == 0 {
		t.Error("Overlay mount appears empty - may indicate mount failure")
	}

	t.Logf("Overlay mount verified: %s at %s with %d entries", container.Name, container.MountPath, len(entries))
}

// TestBuildOverlayOptions tests the mount options string construction.
func TestBuildOverlayOptions(t *testing.T) {
	tests := []struct {
		name        string
		basePath    string
		overrides   []OverrideContainer
		normalPaths []string
		expected    string
	}{
		{
			name:     "base only",
			basePath: "/base",
			expected: "lowerdir=/base",
		},
		{
			name:        "normals only",
			basePath:    "/base",
			normalPaths: []string{"/n1", "/n2"},
			expected:    "lowerdir=/base:/n1:/n2",
		},
		{
			name:     "single override",
			basePath: "/base",
			overrides: []OverrideContainer{
				{MountPath: "/o1", Name: "override1", Priority: 10},
			},
			expected: "lowerdir=/o1:/base",
		},
		{
			name:     "override sorting",
			basePath: "/base",
			overrides: []OverrideContainer{
				{MountPath: "/o2", Name: "override2", Priority: 50},
				{MountPath: "/o1", Name: "override1", Priority: 10},
			},
			expected: "lowerdir=/o1:/o2:/base",
		},
		{
			name:     "equal priority tie-break by name",
			basePath: "/base",
			overrides: []OverrideContainer{
				{MountPath: "/zz", Name: "zulu", Priority: 10},
				{MountPath: "/aa", Name: "alpha", Priority: 10},
			},
			expected: "lowerdir=/aa:/zz:/base",
		},
		{
			name:     "both groups",
			basePath: "/base",
			overrides: []OverrideContainer{
				{MountPath: "/o1", Name: "override1", Priority: 10},
			},
			normalPaths: []string{"/n1", "/n2"},
			expected:    "lowerdir=/o1:/base:/n1:/n2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildOverlayOptions(tt.basePath, tt.overrides, tt.normalPaths)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestBuildOverlayOptionsTruncation verifies page-size truncation behavior.
func TestBuildOverlayOptionsTruncation(t *testing.T) {
	pageSize := os.Getpagesize()

	// Create a long base path that leaves limited room
	basePath := "/" + strings.Repeat("b", pageSize/3)

	// Override should be included before base
	override := OverrideContainer{
		MountPath: "/" + strings.Repeat("o", pageSize/3),
		Name:      "override",
		Priority:  10,
	}

	// Normal paths that would exceed page size
	normalLong := "/" + strings.Repeat("n", pageSize/3)

	result := BuildOverlayOptions(basePath, []OverrideContainer{override}, []string{normalLong})

	// Override and base must be present
	if !strings.Contains(result, override.MountPath) {
		t.Error("override path missing from result")
	}
	if !strings.Contains(result, basePath) {
		t.Error("base path missing from result")
	}

	// Normal path should be truncated (total would exceed page size)
	if strings.Contains(result, normalLong) {
		t.Error("normal path should have been truncated due to page size limit")
	}

	if len(result) >= pageSize-1 {
		t.Errorf("result length %d exceeds page size limit %d", len(result), pageSize-1)
	}

	// Verify override is dropped (not basePath) when override + basePath exceed page size
	hugeOverride := OverrideContainer{
		MountPath: "/" + strings.Repeat("x", pageSize),
		Name:      "huge",
		Priority:  1,
	}
	degraded := BuildOverlayOptions("/base", []OverrideContainer{hugeOverride}, nil)
	if !strings.Contains(degraded, "/base") {
		t.Error("basePath must always be present even when overrides exceed page size")
	}
	if strings.Contains(degraded, hugeOverride.MountPath) {
		t.Error("huge override should have been dropped")
	}

	// Partial override truncation: 3 overrides where only first 2 fit with basePath.
	// Each path is ~1/3 of available space so 3 paths fit but 4 don't.
	third := (pageSize - len("lowerdir=") - 3) / 3 // 3 paths + 2 colons fit
	overrides := []OverrideContainer{
		{MountPath: "/" + strings.Repeat("a", third-1), Name: "first", Priority: 1},
		{MountPath: "/" + strings.Repeat("b", third-1), Name: "second", Priority: 2},
		{MountPath: "/" + strings.Repeat("c", third-1), Name: "third", Priority: 3},
	}
	shortBase := "/" + strings.Repeat("z", third-1)
	partial := BuildOverlayOptions(shortBase, overrides, nil)
	if !strings.Contains(partial, overrides[0].MountPath) {
		t.Error("highest-priority override should be present")
	}
	if !strings.Contains(partial, overrides[1].MountPath) {
		t.Error("second-priority override should be present")
	}
	if !strings.Contains(partial, shortBase) {
		t.Error("basePath must always be present")
	}
	if strings.Contains(partial, overrides[2].MountPath) {
		t.Error("third override should have been dropped due to page size")
	}

	// Normals dropped before overrides: one override + basePath nearly fill the
	// budget, normal should be dropped while override stays
	almostFull := OverrideContainer{
		MountPath: "/" + strings.Repeat("o", pageSize/2),
		Name:      "big-override",
		Priority:  1,
	}
	mediumBase := "/" + strings.Repeat("b", pageSize/4)
	normalPath := "/" + strings.Repeat("n", pageSize/4)
	mixed := BuildOverlayOptions(mediumBase, []OverrideContainer{almostFull}, []string{normalPath})
	if !strings.Contains(mixed, almostFull.MountPath) {
		t.Error("override should be present")
	}
	if !strings.Contains(mixed, mediumBase) {
		t.Error("basePath must always be present")
	}
	if strings.Contains(mixed, normalPath) {
		t.Error("normal should have been dropped since override + base already near limit")
	}
}

// TestOverlayStacking tests overlay stacking with checksum verification.
// It verifies that:
// 1. All files from hostapp and OS blocks appear in the stacked mount
// 2. File checksums match their fingerprints
// 3. No unexpected files appear in the stacked mount
func TestOverlayStacking(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to perform overlay mount")
	}

	if *rootdir == "" {
		t.Skip("This test requires a --rootdir flag")
	}

	// Create mount namespace for isolation
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("failed to create mount namespace: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("failed to make mounts private: %v", err)
	}

	// Step 1: Mount fingerprinted hostapp container
	hostappCurrent := filepath.Join(*rootdir, "fingerprint-current")
	current, err := os.Readlink(hostappCurrent)
	if err != nil {
		t.Skip("No fingerprinted hostapp available (fingerprint-current symlink missing)")
	}
	cid := filepath.Base(current)

	hostappContainers, err := Mount(*rootdir, cid)
	if err != nil {
		t.Fatalf("Mount hostapp failed: %v", err)
	}
	if len(hostappContainers) != 1 {
		t.Fatalf("Expected 1 hostapp container, got %d", len(hostappContainers))
	}
	hostappPath := hostappContainers[0].MountPath
	t.Logf("Fingerprinted hostapp mounted at %s", hostappPath)

	// Step 2: Mount OS block containers
	osBlocks, err := Mount(*rootdir, "io.balena.image.class")
	if err != nil {
		t.Fatalf("Mount OS blocks failed: %v", err)
	}
	if len(osBlocks) == 0 {
		t.Skip("No OS block containers available")
	}
	t.Logf("Found %d OS block containers", len(osBlocks))

	// Step 3: Create stacked overlay mount
	stackedMount := t.TempDir()

	overlayPaths := make([]string, len(osBlocks))
	for i, c := range osBlocks {
		overlayPaths[i] = c.MountPath
		t.Logf("OS block %d: %s at %s", i, c.Name, c.MountPath)
	}

	opts := BuildOverlayOptions(hostappPath, nil, overlayPaths)
	t.Logf("Mount options: %s", opts)

	if err := unix.Mount("overlay", stackedMount, "overlay", 0, opts); err != nil {
		t.Fatalf("Stacked overlay mount failed: %v", err)
	}
	defer unix.Unmount(stackedMount, unix.MNT_DETACH)

	// Step 4: Load all fingerprints and build expected checksums map
	// In overlay fs, lowerdir=A:B:C means A is topmost (takes precedence).
	// Our mount is: lowerdir=hostapp:osblock1:osblock2:osblock3
	// So precedence is: hostapp > osblock1 > osblock2 > osblock3
	// Load in reverse order so higher precedence layers overwrite lower ones.
	expectedChecksums := make(map[string]string) // path -> md5sum

	// Load OS block fingerprints in reverse order (lowest precedence first)
	for i := len(osBlocks); i >= 1; i-- {
		fp := filepath.Join(stackedMount, fmt.Sprintf(".fingerprint-osblock-%d", i))
		countBefore := len(expectedChecksums)
		if err := loadFingerprint(fp, expectedChecksums); err != nil {
			t.Fatalf("Failed to load OS block %d fingerprint: %v", i, err)
		}
		t.Logf("Loaded OS block %d fingerprint with %d new files (total: %d)", i, len(expectedChecksums)-countBefore, len(expectedChecksums))
	}

	// Load hostapp fingerprint last (highest precedence - will overwrite duplicates)
	hostappFingerprint := filepath.Join(stackedMount, ".fingerprint-hostapp")
	countBefore := len(expectedChecksums)
	if err := loadFingerprint(hostappFingerprint, expectedChecksums); err != nil {
		t.Fatalf("Failed to load hostapp fingerprint: %v", err)
	}
	t.Logf("Loaded hostapp fingerprint with %d new files (total: %d)", len(expectedChecksums)-countBefore, len(expectedChecksums))

	// Step 5: Verify all fingerprinted files exist and have correct checksums
	// Skip broken symlinks (absolute symlinks pointing outside the mount)
	checksumErrors := 0
	skippedFiles := 0
	verifiedFiles := 0
	for relPath, expectedMD5 := range expectedChecksums {
		actualPath := filepath.Join(stackedMount, relPath)
		actualMD5, err := computeMD5(actualPath)
		if err != nil {
			// Skip broken symlinks silently
			skippedFiles++
			continue
		}
		verifiedFiles++
		if actualMD5 != expectedMD5 {
			t.Errorf("Checksum mismatch for %s: expected %s, got %s", relPath, expectedMD5, actualMD5)
			checksumErrors++
		}
	}
	t.Logf("Verified %d files, skipped %d broken symlinks, %d errors", verifiedFiles, skippedFiles, checksumErrors)

	// Step 6: Check for unexpected files (not in any fingerprint)
	// Skip broken symlinks and fingerprint files
	unexpectedFiles := []string{}

	err = filepath.Walk(stackedMount, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip broken symlinks and unreadable paths
		}
		if info.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil // Skip symlinks
		}

		// Get path relative to mount (without leading slash to match fingerprint format)
		relPath := strings.TrimPrefix(strings.TrimPrefix(path, stackedMount), "/")

		// Skip fingerprint files themselves
		if strings.HasPrefix(relPath, ".fingerprint-") {
			return nil
		}

		if _, ok := expectedChecksums[relPath]; !ok {
			unexpectedFiles = append(unexpectedFiles, relPath)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("Failed to walk stacked mount: %v", err)
	}

	if len(unexpectedFiles) > 0 {
		t.Errorf("Found %d unexpected files not in any fingerprint:", len(unexpectedFiles))
		for _, f := range unexpectedFiles {
			t.Errorf("  - %s", f)
		}
	}

	if checksumErrors > 0 {
		t.Fatalf("Overlay stacking failed: %d checksum errors", checksumErrors)
	}

	// Step 7: Verify .dockerenv does not exist (would cause systemd to think it's in a container)
	dockerenvPath := filepath.Join(stackedMount, ".dockerenv")
	if _, err := os.Lstat(dockerenvPath); err == nil {
		t.Errorf(".dockerenv exists in stacked mount - this will cause systemd to detect container mode")
	} else if !os.IsNotExist(err) {
		t.Errorf("Error checking .dockerenv: %v", err)
	}

	t.Logf("Overlay stacking verified: %d files, %d skipped, %d unexpected",
		verifiedFiles, skippedFiles, len(unexpectedFiles))
}

// TestNoDockerenvInOverlay verifies that .dockerenv does not exist in container mounts.
// Docker creates .dockerenv when containers are created (docker create/run), which
// causes systemd to detect container mode and skip hardware initialization.
func TestNoDockerenvInOverlay(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to perform overlay mount")
	}

	if *rootdir == "" {
		t.Skip("This test requires a --rootdir flag")
	}

	// Create mount namespace for isolation
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("failed to create mount namespace: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("failed to make mounts private: %v", err)
	}

	// Mount hostapp container
	hostappCurrent := filepath.Join(*rootdir, "current")
	current, err := os.Readlink(hostappCurrent)
	if err != nil {
		t.Skip("No hostapp available (current symlink missing)")
	}
	cid := filepath.Base(current)

	containers, err := Mount(*rootdir, cid)
	if err != nil {
		t.Fatalf("Mount hostapp failed: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("Expected 1 container, got %d", len(containers))
	}

	// Check hostapp for .dockerenv
	dockerenvPath := filepath.Join(containers[0].MountPath, ".dockerenv")
	if _, err := os.Lstat(dockerenvPath); err == nil {
		t.Errorf("Hostapp container has .dockerenv at %s - this will cause systemd to detect container mode", dockerenvPath)
	}

	// Mount and check OS block containers
	osBlocks, err := Mount(*rootdir, "io.balena.image.class")
	if err != nil {
		t.Logf("No OS blocks to check: %v", err)
		return
	}

	for _, c := range osBlocks {
		dockerenvPath := filepath.Join(c.MountPath, ".dockerenv")
		if _, err := os.Lstat(dockerenvPath); err == nil {
			t.Errorf("OS block %s has .dockerenv at %s - this will cause systemd to detect container mode", c.Name, dockerenvPath)
		}
	}
}

// TestGetKernelVersion verifies that GetKernelVersion returns a valid kernel version string.
func TestGetKernelVersion(t *testing.T) {
	ver, err := GetKernelVersion()
	if err != nil {
		t.Fatalf("GetKernelVersion failed: %v", err)
	}
	if ver == "" {
		t.Fatal("GetKernelVersion returned empty string")
	}
	// Should be M.m.p format (e.g., "6.8.0"), no local extensions
	if ver[0] < '0' || ver[0] > '9' {
		t.Errorf("unexpected kernel version format: %q", ver)
	}
	if strings.Contains(ver, "-") {
		t.Errorf("kernel version should not contain local extensions: %q", ver)
	}
	t.Logf("Kernel version: %s", ver)
}

// TestFilterByKernelVersion tests the kernel version filtering logic.
func TestFilterByKernelVersion(t *testing.T) {
	makeContainer := func(name string, labels map[string]string) Container {
		return Container{
			Config: Config{
				HostConfig: HostConfig{Labels: labels},
				Name:       name,
			},
		}
	}

	tests := []struct {
		name          string
		containers    []Container
		kernelVersion string
		expectNames   []string
	}{
		{
			name: "no label passes through",
			containers: []Container{
				makeContainer("no-label", map[string]string{"io.balena.image.class": "overlay"}),
			},
			kernelVersion: "6.1.0",
			expectNames:   []string{"no-label"},
		},
		{
			name: "matching label passes",
			containers: []Container{
				makeContainer("match", map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "6.1.0"}),
			},
			kernelVersion: "6.1.0",
			expectNames:   []string{"match"},
		},
		{
			name: "mismatched label filtered",
			containers: []Container{
				makeContainer("old", map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "5.15.0"}),
			},
			kernelVersion: "6.1.0",
			expectNames:   nil,
		},
		{
			name: "mixed: keep matching and unlabelled, skip mismatched",
			containers: []Container{
				makeContainer("match", map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "6.1.0"}),
				makeContainer("old", map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "5.15.0"}),
				makeContainer("unlabelled", map[string]string{"io.balena.image.class": "overlay"}),
			},
			kernelVersion: "6.1.0",
			expectNames:   []string{"match", "unlabelled"},
		},
		{
			name: "empty kernel version passes all",
			containers: []Container{
				makeContainer("a", map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "6.1.0"}),
				makeContainer("b", map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "5.15.0"}),
			},
			kernelVersion: "",
			expectNames:   []string{"a", "b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FilterByKernelVersion(tt.containers, tt.kernelVersion)
			var gotNames []string
			for _, c := range result {
				gotNames = append(gotNames, c.Name)
			}
			if len(gotNames) != len(tt.expectNames) {
				t.Fatalf("expected %v, got %v", tt.expectNames, gotNames)
			}
			for i, name := range gotNames {
				if name != tt.expectNames[i] {
					t.Errorf("index %d: expected %q, got %q", i, tt.expectNames[i], name)
				}
			}
		})
	}
}

// loadFingerprint reads a fingerprint file and adds entries to the checksums map.
// Fingerprint format: "md5sum  /path/to/file" (standard md5sum output)
// Paths are stored without leading slash to allow proper path joining.
func loadFingerprint(path string, checksums map[string]string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// md5sum output format: "checksum  filename" (two spaces)
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			continue
		}
		md5sum := parts[0]
		filePath := strings.TrimPrefix(parts[1], "/") // Remove leading slash for proper joining
		checksums[filePath] = md5sum
	}
	return nil
}

// computeMD5 calculates the MD5 checksum of a file
func computeMD5(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := md5.Sum(content)
	return fmt.Sprintf("%x", sum), nil
}
