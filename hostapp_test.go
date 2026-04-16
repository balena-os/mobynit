package hostapp

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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
		name            string
		basePath        string
		leftExtensions  []Extension
		rightExtensions []Extension
		expected        string
	}{
		{
			name:     "base only",
			basePath: "/base",
			expected: "lowerdir=/base",
		},
		{
			name:     "rights only",
			basePath: "/base",
			rightExtensions: []Extension{
				{MountPath: "/n1", Name: "right1"},
				{MountPath: "/n2", Name: "right2"},
			},
			expected: "lowerdir=/base:/n1:/n2",
		},
		{
			name:     "single left",
			basePath: "/base",
			leftExtensions: []Extension{
				{MountPath: "/o1", Name: "left1", Priority: 10},
			},
			expected: "lowerdir=/o1:/base",
		},
		{
			name:     "left sorting",
			basePath: "/base",
			leftExtensions: []Extension{
				{MountPath: "/o2", Name: "left2", Priority: 50},
				{MountPath: "/o1", Name: "left1", Priority: 10},
			},
			expected: "lowerdir=/o1:/o2:/base",
		},
		{
			name:     "equal priority tie-break by name",
			basePath: "/base",
			leftExtensions: []Extension{
				{MountPath: "/zz", Name: "zulu", Priority: 10},
				{MountPath: "/aa", Name: "alpha", Priority: 10},
			},
			expected: "lowerdir=/aa:/zz:/base",
		},
		{
			name:     "both sides",
			basePath: "/base",
			leftExtensions: []Extension{
				{MountPath: "/o1", Name: "left1", Priority: 10},
			},
			rightExtensions: []Extension{
				{MountPath: "/n1", Name: "right1"},
				{MountPath: "/n2", Name: "right2"},
			},
			expected: "lowerdir=/o1:/base:/n1:/n2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildOverlayOptions(tt.basePath, tt.leftExtensions, tt.rightExtensions)
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

	// Left extension should be included before base
	left := Extension{
		MountPath: "/" + strings.Repeat("o", pageSize/3),
		Name:      "left",
		Priority:  10,
	}

	// Right extension that would exceed page size
	rightLong := Extension{
		MountPath: "/" + strings.Repeat("n", pageSize/3),
		Name:      "right",
	}

	result := BuildOverlayOptions(basePath, []Extension{left}, []Extension{rightLong})

	// Left extension and base must be present
	if !strings.Contains(result, left.MountPath) {
		t.Error("left extension path missing from result")
	}
	if !strings.Contains(result, basePath) {
		t.Error("base path missing from result")
	}

	// Right extension should be truncated (total would exceed page size)
	if strings.Contains(result, rightLong.MountPath) {
		t.Error("right extension path should have been truncated due to page size limit")
	}

	if len(result) >= pageSize-1 {
		t.Errorf("result length %d exceeds page size limit %d", len(result), pageSize-1)
	}

	// Verify left extension is dropped (not basePath) when it + basePath exceed page size
	hugeLeft := Extension{
		MountPath: "/" + strings.Repeat("x", pageSize),
		Name:      "huge",
		Priority:  1,
	}
	degraded := BuildOverlayOptions("/base", []Extension{hugeLeft}, nil)
	if !strings.Contains(degraded, "/base") {
		t.Error("basePath must always be present even when left extensions exceed page size")
	}
	if strings.Contains(degraded, hugeLeft.MountPath) {
		t.Error("huge left extension should have been dropped")
	}

	// Partial left truncation: 3 left extensions where only first 2 fit with basePath.
	// Each path is ~1/3 of available space so 3 paths fit but 4 don't.
	third := (pageSize - len("lowerdir=") - 3) / 3 // 3 paths + 2 colons fit
	lefts := []Extension{
		{MountPath: "/" + strings.Repeat("a", third-1), Name: "first", Priority: 1},
		{MountPath: "/" + strings.Repeat("b", third-1), Name: "second", Priority: 2},
		{MountPath: "/" + strings.Repeat("c", third-1), Name: "third", Priority: 3},
	}
	shortBase := "/" + strings.Repeat("z", third-1)
	partial := BuildOverlayOptions(shortBase, lefts, nil)
	if !strings.Contains(partial, lefts[0].MountPath) {
		t.Error("highest-priority left extension should be present")
	}
	if !strings.Contains(partial, lefts[1].MountPath) {
		t.Error("second-priority left extension should be present")
	}
	if !strings.Contains(partial, shortBase) {
		t.Error("basePath must always be present")
	}
	if strings.Contains(partial, lefts[2].MountPath) {
		t.Error("third left extension should have been dropped due to page size")
	}

	// Right extensions dropped before left extensions: one left + basePath nearly
	// fill the budget, right should be dropped while left stays
	almostFull := Extension{
		MountPath: "/" + strings.Repeat("o", pageSize/2),
		Name:      "big-left",
		Priority:  1,
	}
	mediumBase := "/" + strings.Repeat("b", pageSize/4)
	rightExt := Extension{
		MountPath: "/" + strings.Repeat("n", pageSize/4),
		Name:      "right",
	}
	mixed := BuildOverlayOptions(mediumBase, []Extension{almostFull}, []Extension{rightExt})
	if !strings.Contains(mixed, almostFull.MountPath) {
		t.Error("left extension should be present")
	}
	if !strings.Contains(mixed, mediumBase) {
		t.Error("basePath must always be present")
	}
	if strings.Contains(mixed, rightExt.MountPath) {
		t.Error("right extension should have been dropped since left + base already near limit")
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

	rightExtensions := make([]Extension, len(osBlocks))
	for i, c := range osBlocks {
		rightExtensions[i] = Extension{Name: c.Name, MountPath: c.MountPath}
		t.Logf("OS block %d: %s at %s", i, c.Name, c.MountPath)
	}

	opts := BuildOverlayOptions(hostappPath, nil, rightExtensions)
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

// makeTestContainer builds a Container for table-driven filter tests.
func makeTestContainer(name string, labels map[string]string) Container {
	return Container{
		Config: Config{
			HostConfig: HostConfig{Labels: labels},
			Name:       name,
		},
	}
}

func TestGetKernelRelease(t *testing.T) {
	release, err := GetKernelRelease()
	if err != nil {
		t.Fatalf("GetKernelRelease failed: %v", err)
	}
	if release == "" {
		t.Fatal("expected non-empty release")
	}
	if strings.ContainsRune(release, 0) {
		t.Errorf("release contains NUL byte: %q", release)
	}
}

func TestKernelVersionFromRelease(t *testing.T) {
	tests := []struct {
		release string
		want    string
	}{
		{"6.8.0-100-generic", "6.8.0"},
		{"6.1.0-v8+", "6.1.0"},
		{"5.15.0", "5.15.0"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := kernelVersionFromRelease(tt.release); got != tt.want {
			t.Errorf("kernelVersionFromRelease(%q) = %q, want %q", tt.release, got, tt.want)
		}
	}
}

func TestFilterByKernelVersion(t *testing.T) {
	tests := []struct {
		name          string
		containers    []Container
		kernelVersion string
		expectNames   []string
	}{
		{
			name: "no label passes through",
			containers: []Container{
				makeTestContainer("no-label", map[string]string{"io.balena.image.class": "overlay"}),
			},
			kernelVersion: "6.1.0",
			expectNames:   []string{"no-label"},
		},
		{
			name: "matching label passes",
			containers: []Container{
				makeTestContainer("match", map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "6.1.0"}),
			},
			kernelVersion: "6.1.0",
			expectNames:   []string{"match"},
		},
		{
			name: "mismatched label filtered",
			containers: []Container{
				makeTestContainer("old", map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "5.15.0"}),
			},
			kernelVersion: "6.1.0",
			expectNames:   nil,
		},
		{
			name: "mixed: keep matching and unlabelled, skip mismatched",
			containers: []Container{
				makeTestContainer("match", map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "6.1.0"}),
				makeTestContainer("old", map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "5.15.0"}),
				makeTestContainer("unlabelled", map[string]string{"io.balena.image.class": "overlay"}),
			},
			kernelVersion: "6.1.0",
			expectNames:   []string{"match", "unlabelled"},
		},
		{
			name: "empty kernel version passes all",
			containers: []Container{
				makeTestContainer("a", map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "6.1.0"}),
				makeTestContainer("b", map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "5.15.0"}),
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
			if !reflect.DeepEqual(gotNames, tt.expectNames) {
				t.Errorf("expected %v, got %v", tt.expectNames, gotNames)
			}
		})
	}
}

func TestComputeABIID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Module.symvers")
	content := []byte("0xdeadbeef\tsome_symbol\tvmlinux\tEXPORT_SYMBOL\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	expected := sha256.Sum256(content)
	want := hex.EncodeToString(expected[:])

	got, err := ComputeABIID(path)
	if err != nil {
		t.Fatalf("ComputeABIID: %v", err)
	}
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}

	if _, err := ComputeABIID(filepath.Join(dir, "nope")); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestParseHostKernelABIID(t *testing.T) {
	abi := strings.Repeat("a", 64)
	tests := []struct {
		name    string
		cmdline string
		want    string
	}{
		{"absent", "console=ttyS0 root=UUID=xxx rootwait", ""},
		{"empty", "", ""},
		{"present alone", "balena_kernel_abi=" + abi, abi},
		{"present among others", "console=ttyS0 balena_kernel_abi=" + abi + " rootwait", abi},
		{"trailing newline", "ro balena_kernel_abi=" + abi + "\n", abi},
		{"empty value", "ro balena_kernel_abi= rootwait", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseHostKernelABIID(tt.cmdline); got != tt.want {
				t.Errorf("ParseHostKernelABIID(%q) = %q, want %q", tt.cmdline, got, tt.want)
			}
		})
	}
}

// writeConfigV2 writes the given root map as config.v2.json in a temp home
// directory and returns a Container with HomePath populated. The Labels map
// is linked to the one written into the JSON so in-memory state matches disk.
func writeConfigV2(t *testing.T, name string, labels map[string]string, extra map[string]interface{}) Container {
	t.Helper()
	home := t.TempDir()
	jsonLabels := map[string]interface{}{}
	for k, v := range labels {
		jsonLabels[k] = v
	}
	cfg := map[string]interface{}{
		"Labels": jsonLabels,
	}
	if extra != nil {
		if cfgExtra, ok := extra["Config"].(map[string]interface{}); ok {
			for k, v := range cfgExtra {
				cfg[k] = v
			}
		}
	}
	root := map[string]interface{}{
		"ID":     "cid-" + name,
		"Name":   name,
		"Driver": "overlay2",
		"Config": cfg,
	}
	if extra != nil {
		for k, v := range extra {
			if k == "Config" {
				continue
			}
			root[k] = v
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.v2.json"), out, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c := Container{
		Config: Config{
			HostConfig: HostConfig{Labels: map[string]string{}},
			Name:       name,
			ID:         "cid-" + name,
			Driver:     "overlay2",
		},
		HomePath: home,
	}
	for k, v := range labels {
		c.Labels[k] = v
	}
	return c
}

// writeModuleSymvers creates <mount>/lib/modules/<release>/Module.symvers with
// the given content under a fresh temp dir, sets it as the container's
// MountPath, and returns the sha256 the resolver must compute.
func writeModuleSymvers(t *testing.T, c *Container, release string, content []byte) string {
	t.Helper()
	mount := t.TempDir()
	modDir := filepath.Join(mount, "lib", "modules", release)
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "Module.symvers"), content, 0644); err != nil {
		t.Fatal(err)
	}
	c.MountPath = mount
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func TestResolveExtensionABIID(t *testing.T) {
	// A fixed release decouples the fixtures from the host's running kernel.
	const release = "6.1.0-test"

	t.Run("empty MountPath returns empty (agnostic)", func(t *testing.T) {
		c := writeConfigV2(t, "no-mount", nil, nil)
		// MountPath deliberately left empty.
		got, err := c.ResolveExtensionABIID(release)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("no /lib/modules returns empty (agnostic)", func(t *testing.T) {
		c := writeConfigV2(t, "no-modules", nil, nil)
		c.MountPath = t.TempDir()
		got, err := c.ResolveExtensionABIID(release)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("empty release on an agnostic extension passes (no ABI claim)", func(t *testing.T) {
		c := writeConfigV2(t, "agnostic-unknown-release", nil, nil)
		c.MountPath = t.TempDir()
		got, err := c.ResolveExtensionABIID("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("empty release on a module-carrying extension is an error (fail-closed)", func(t *testing.T) {
		c := writeConfigV2(t, "kernel-unknown-release", nil, nil)
		writeModuleSymvers(t, &c, release, []byte("symvers-fixture\n"))
		if _, err := c.ResolveExtensionABIID(""); err == nil {
			t.Error("expected error when running kernel release is unknown")
		}
	})

	t.Run("modules dir without Module.symvers is an error (broken)", func(t *testing.T) {
		c := writeConfigV2(t, "broken", nil, nil)
		mount := t.TempDir()
		if err := os.MkdirAll(filepath.Join(mount, "lib", "modules", release), 0755); err != nil {
			t.Fatal(err)
		}
		c.MountPath = mount
		if _, err := c.ResolveExtensionABIID(release); err == nil {
			t.Error("expected error for missing Module.symvers")
		}
	})

	t.Run("Module.symvers present computes sha without writing it back", func(t *testing.T) {
		c := writeConfigV2(t, "disco", nil, nil)
		want := writeModuleSymvers(t, &c, release, []byte("ext-symvers-fixture\n"))

		got, err := c.ResolveExtensionABIID(release)
		if err != nil {
			t.Fatalf("ResolveExtensionABIID: %v", err)
		}
		if got != want {
			t.Errorf("expected %q, got %q", want, got)
		}
		// The label is not persisted: the build bakes it onto the image, so
		// mobynit must not mutate config.v2.json in the early-boot path.
		if _, ok := c.Labels[HOSTOS_BLOCKS_KERNEL_ABI_ID]; ok {
			t.Errorf("in-memory label should not be set, got %v", c.Labels)
		}
		raw, _ := os.ReadFile(filepath.Join(c.HomePath, "config.v2.json"))
		var root map[string]interface{}
		_ = json.Unmarshal(raw, &root)
		labels, _ := root["Config"].(map[string]interface{})["Labels"].(map[string]interface{})
		if _, ok := labels[HOSTOS_BLOCKS_KERNEL_ABI_ID]; ok {
			t.Errorf("config.v2.json should not be written back, got labels %v", labels)
		}
	})

	t.Run("label matching computed value passes", func(t *testing.T) {
		c := writeConfigV2(t, "consistent", nil, nil)
		want := writeModuleSymvers(t, &c, release, []byte("consistent-fixture\n"))
		c.Labels[HOSTOS_BLOCKS_KERNEL_ABI_ID] = want

		got, err := c.ResolveExtensionABIID(release)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Errorf("expected %q, got %q", want, got)
		}
	})

	t.Run("label disagreeing with computed value is an error", func(t *testing.T) {
		c := writeConfigV2(t, "inconsistent", nil, nil)
		writeModuleSymvers(t, &c, release, []byte("inconsistent-fixture\n"))
		c.Labels[HOSTOS_BLOCKS_KERNEL_ABI_ID] = "deadbeef-bogus-label"

		if _, err := c.ResolveExtensionABIID(release); err == nil {
			t.Error("expected error when label disagrees with computed sha256")
		}
	})
}

// abiKind selects how buildFilterContainer provisions an extension's mount.
type abiKind int

const (
	abiAgnostic abiKind = iota // no /lib/modules/<release>: makes no ABI claim
	abiBroken                  // /lib/modules/<release> present but Module.symvers missing
	abiKernel                  // Module.symvers present, ABI = sha256(content)
)

// buildFilterContainer materialises a container exactly as FilterByKernelABIID
// will see it: ABI-agnostic, broken, or kernel-carrying with a known symvers
// content (so the caller can compute the matching host ABI).
func buildFilterContainer(t *testing.T, name string, release string, kind abiKind, symvers []byte) Container {
	t.Helper()
	c := writeConfigV2(t, name, nil, nil)
	switch kind {
	case abiAgnostic:
		c.MountPath = t.TempDir()
	case abiBroken:
		mount := t.TempDir()
		if err := os.MkdirAll(filepath.Join(mount, "lib", "modules", release), 0755); err != nil {
			t.Fatal(err)
		}
		c.MountPath = mount
	case abiKernel:
		writeModuleSymvers(t, &c, release, symvers)
	}
	return c
}

func abiOf(symvers []byte) string {
	sum := sha256.Sum256(symvers)
	return hex.EncodeToString(sum[:])
}

func TestFilterByKernelABIID(t *testing.T) {
	const release = "6.1.0-test"

	abiA := []byte("abi-A-symvers\n")
	abiB := []byte("abi-B-symvers\n")
	hostA := abiOf(abiA)

	tests := []struct {
		name        string
		containers  []Container
		hostABIID   string
		expectNames []string
	}{
		{
			name: "agnostic extension passes when host ABI present",
			containers: []Container{
				buildFilterContainer(t, "agnostic", release, abiAgnostic, nil),
			},
			hostABIID:   hostA,
			expectNames: []string{"agnostic"},
		},
		{
			name: "matching ABI mounts",
			containers: []Container{
				buildFilterContainer(t, "match", release, abiKernel, abiA),
			},
			hostABIID:   hostA,
			expectNames: []string{"match"},
		},
		{
			name: "mismatched ABI skipped",
			containers: []Container{
				buildFilterContainer(t, "wrong-abi", release, abiKernel, abiB),
			},
			hostABIID:   hostA,
			expectNames: nil,
		},
		{
			name: "broken extension skipped (fail-closed)",
			containers: []Container{
				buildFilterContainer(t, "broken", release, abiBroken, nil),
			},
			hostABIID:   hostA,
			expectNames: nil,
		},
		{
			name: "absent host ABI skips kernel block but passes agnostic (fail-closed)",
			containers: []Container{
				buildFilterContainer(t, "kernel", release, abiKernel, abiA),
				buildFilterContainer(t, "agnostic", release, abiAgnostic, nil),
			},
			hostABIID:   "",
			expectNames: []string{"agnostic"},
		},
		{
			name: "mixed: only the ABI-matching kernel block and agnostic pass",
			containers: []Container{
				buildFilterContainer(t, "match", release, abiKernel, abiA),
				buildFilterContainer(t, "wrong-abi", release, abiKernel, abiB),
				buildFilterContainer(t, "agnostic", release, abiAgnostic, nil),
			},
			hostABIID:   hostA,
			expectNames: []string{"match", "agnostic"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FilterByKernelABIID(tt.containers, release, tt.hostABIID)
			var gotNames []string
			for _, c := range result {
				gotNames = append(gotNames, c.Name)
			}
			if !reflect.DeepEqual(gotNames, tt.expectNames) {
				t.Errorf("expected %v, got %v", tt.expectNames, gotNames)
			}
		})
	}
}

// pathIsMounted reports whether path is a mount point, by comparing its st_dev
// to its parent's (the technique mountpoint(1) uses). This resolves the path in
// the calling thread's mount namespace, so it works under CLONE_NEWNS: unlike
// /proc/self/mountinfo, which reflects the thread-group leader's namespace, not
// the unshared test thread's.
func pathIsMounted(t *testing.T, path string) bool {
	t.Helper()
	var st, parent unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if err := unix.Lstat(filepath.Dir(path), &parent); err != nil {
		t.Fatalf("lstat %s: %v", filepath.Dir(path), err)
	}
	return st.Dev != parent.Dev
}

// TestSelectMountable verifies that SelectMountable releases the overlay
// mounts of dropped candidates while leaving the selected set mounted.
func TestSelectMountable(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to mount/unmount")
	}
	// Pin to one OS thread so the unshare, the mounts, and the mountinfo read
	// all run in the same (unshared) mount namespace; otherwise the Go
	// scheduler can migrate the goroutine across threads in different namespaces.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Isolate so the test's mounts never leak into the host namespace.
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("unshare: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("make mounts private: %v", err)
	}

	mkMount := func(name string) string {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := unix.Mount("tmpfs", dir, "tmpfs", 0, ""); err != nil {
			t.Fatalf("mounting tmpfs at %s: %v", dir, err)
		}
		return dir
	}

	keepPath := mkMount("keep")
	dropPath := mkMount("drop")
	// Best-effort teardown (dropPath is already unmounted by the call under test).
	defer unix.Unmount(keepPath, unix.MNT_DETACH)
	defer unix.Unmount(dropPath, unix.MNT_DETACH)

	// The running release is 6.0.0; dropid's kernel-version label (0.0.0)
	// mismatches, so SelectMountable drops and unmounts it; keepid is unlabelled
	// and survives. Neither carries kernel modules, so the ABI filter passes both.
	all := []Container{
		{Config: Config{ID: "keepid", Name: "keep"}, MountPath: keepPath},
		{Config: Config{
			ID:         "dropid",
			Name:       "drop",
			HostConfig: HostConfig{Labels: map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "0.0.0"}},
		}, MountPath: dropPath},
	}

	selected := SelectMountable(all, "6.0.0", "")
	if len(selected) != 1 || selected[0].ID != "keepid" {
		t.Fatalf("expected only keepid selected, got %+v", selected)
	}

	// Selected candidate stays mounted and keeps its MountPath.
	if !pathIsMounted(t, keepPath) {
		t.Errorf("selected overlay should still be mounted at %s", keepPath)
	}
	if all[0].MountPath != keepPath {
		t.Errorf("selected MountPath changed: %q", all[0].MountPath)
	}
	// Dropped candidate is unmounted and its MountPath cleared.
	if pathIsMounted(t, dropPath) {
		t.Errorf("dropped overlay should have been unmounted at %s", dropPath)
	}
	if all[1].MountPath != "" {
		t.Errorf("dropped MountPath should be cleared, got %q", all[1].MountPath)
	}
}
