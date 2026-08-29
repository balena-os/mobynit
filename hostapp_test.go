package hostapp

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// fakeOverlay2Layer creates an overlay2 layer directory <overlay2Dir>/<id>
// with a diff/ subdir, a link file naming its short id, and the matching
// l/<short> -> ../<id>/diff symlink the engine maintains. It returns the
// short id so callers can wire up lower chains.
func fakeOverlay2Layer(t *testing.T, overlay2Dir, id, short string) string {
	t.Helper()
	layerDir := filepath.Join(overlay2Dir, id)
	if err := os.MkdirAll(filepath.Join(layerDir, "diff"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layerDir, "link"), []byte(short), 0644); err != nil {
		t.Fatal(err)
	}
	lDir := filepath.Join(overlay2Dir, "l")
	if err := os.MkdirAll(lDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", id, "diff"), filepath.Join(lDir, short)); err != nil {
		t.Fatal(err)
	}
	return short
}

// TestBuildLowerDirs verifies the lowerdir list references every layer by the
// engine's compact overlay2/l/<short> symlink rather than the resolved
// overlay2/<64-hex>/diff path.
func TestBuildLowerDirs(t *testing.T) {
	overlay2Dir := t.TempDir()

	topID := strings.Repeat("a", 64)
	parentID := strings.Repeat("b", 64)
	initID := strings.Repeat("c", 64)

	fakeOverlay2Layer(t, overlay2Dir, topID, "TOPSHORTLINKAAAAAAAAAAAAAA")
	fakeOverlay2Layer(t, overlay2Dir, parentID, "PARENTSHORTLINKBBBBBBBBBBB")
	// The init layer's directory carries the -init suffix; its short link
	// target is what classifies it, without resolving the full path.
	fakeOverlay2Layer(t, overlay2Dir, initID+"-init", "INITSHORTLINKCCCCCCCCCCCCC")

	topLayerDir := filepath.Join(overlay2Dir, topID)
	// lower lists the immediate parent (the init layer) first, then image layers.
	lower := "l/INITSHORTLINKCCCCCCCCCCCCC:l/PARENTSHORTLINKBBBBBBBBBBB"
	if err := os.WriteFile(filepath.Join(topLayerDir, "lower"), []byte(lower), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := buildLowerDirs(overlay2Dir, topLayerDir)
	if err != nil {
		t.Fatalf("buildLowerDirs: %v", err)
	}

	want := []string{
		filepath.Join(overlay2Dir, "l", "TOPSHORTLINKAAAAAAAAAAAAAA"),
		filepath.Join(overlay2Dir, "l", "PARENTSHORTLINKBBBBBBBBBBB"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildLowerDirs = %v, want %v", got, want)
	}

	// The compact form must not carry any resolved /diff path: that is the
	// whole point of the page-budget fix.
	for _, d := range got {
		if strings.HasSuffix(d, "/diff") {
			t.Errorf("lowerdir %q is a resolved diff path, expected compact l/<short> form", d)
		}
	}
}

// TestBuildLowerDirsSingleLayer covers an image with no lower file (one layer):
// only the top layer's compact reference is returned.
func TestBuildLowerDirsSingleLayer(t *testing.T) {
	overlay2Dir := t.TempDir()
	topID := strings.Repeat("a", 64)
	fakeOverlay2Layer(t, overlay2Dir, topID, "TOPSHORTLINKAAAAAAAAAAAAAA")

	got, err := buildLowerDirs(overlay2Dir, filepath.Join(overlay2Dir, topID))
	if err != nil {
		t.Fatalf("buildLowerDirs: %v", err)
	}
	want := []string{filepath.Join(overlay2Dir, "l", "TOPSHORTLINKAAAAAAAAAAAAAA")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildLowerDirs = %v, want %v", got, want)
	}
}

// TestBuildLowerDirsMountsUnderKernel proves the kernel accepts the compact
// l/<short> symlink lowerdir that buildLowerDirs produces.
func TestBuildLowerDirsMountsUnderKernel(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (or unshare -rm) to perform overlay mount")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("unshare: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("make mounts private: %v", err)
	}

	overlay2Dir := t.TempDir()
	topID := strings.Repeat("a", 64)
	parentID := strings.Repeat("b", 64)
	initID := strings.Repeat("c", 64)

	fakeOverlay2Layer(t, overlay2Dir, topID, "TOPSHORTLINKAAAAAAAAAAAAAA")
	fakeOverlay2Layer(t, overlay2Dir, parentID, "PARENTSHORTLINKBBBBBBBBBBB")
	fakeOverlay2Layer(t, overlay2Dir, initID+"-init", "INITSHORTLINKCCCCCCCCCCCCC")

	// Distinct content per layer so the merged view proves each was stacked.
	write := func(id, name, content string) {
		if err := os.WriteFile(filepath.Join(overlay2Dir, id, "diff", name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(topID, "top.txt", "from-top")
	write(parentID, "parent.txt", "from-parent")
	// The init layer ships a file that must NOT appear: it is dropped.
	write(initID+"-init", ".dockerenv", "")

	topLayerDir := filepath.Join(overlay2Dir, topID)
	lower := "l/INITSHORTLINKCCCCCCCCCCCCC:l/PARENTSHORTLINKBBBBBBBBBBB"
	if err := os.WriteFile(filepath.Join(topLayerDir, "lower"), []byte(lower), 0644); err != nil {
		t.Fatal(err)
	}

	lowerDirs, err := buildLowerDirs(overlay2Dir, topLayerDir)
	if err != nil {
		t.Fatalf("buildLowerDirs: %v", err)
	}

	mnt := t.TempDir()
	opts := "lowerdir=" + strings.Join(lowerDirs, ":")
	if err := unix.Mount("overlay", mnt, "overlay", 0, opts); err != nil {
		t.Fatalf("overlay mount rejected by kernel with compact symlink lowerdir %q: %v", opts, err)
	}
	defer unix.Unmount(mnt, unix.MNT_DETACH)

	for name, want := range map[string]string{"top.txt": "from-top", "parent.txt": "from-parent"} {
		got, err := os.ReadFile(filepath.Join(mnt, name))
		if err != nil {
			t.Errorf("reading %s from merged overlay: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// The init layer was dropped, so its .dockerenv must not surface.
	if _, err := os.Lstat(filepath.Join(mnt, ".dockerenv")); !os.IsNotExist(err) {
		t.Errorf(".dockerenv leaked from init layer into merged overlay (err=%v)", err)
	}
}

// TestBuildOverlayOptions tests the flat mount options string construction.
func TestBuildOverlayOptions(t *testing.T) {
	tests := []struct {
		name            string
		baseLayers      []string
		leftExtensions  []Extension
		rightExtensions []Extension
		expected        string
	}{
		{
			name:       "base only",
			baseLayers: []string{"/b1", "/b2"},
			expected:   "lowerdir=/b1:/b2",
		},
		{
			name:       "rights only",
			baseLayers: []string{"/base"},
			rightExtensions: []Extension{
				{Layers: []string{"/n1a", "/n1b"}, Name: "right1"},
				{Layers: []string{"/n2"}, Name: "right2"},
			},
			expected: "lowerdir=/base:/n1a:/n1b:/n2",
		},
		{
			name:       "single left multi-layer",
			baseLayers: []string{"/base"},
			leftExtensions: []Extension{
				{Layers: []string{"/o1a", "/o1b"}, Name: "left1", Priority: 10},
			},
			expected: "lowerdir=/o1a:/o1b:/base",
		},
		{
			name:       "left sorting",
			baseLayers: []string{"/base"},
			leftExtensions: []Extension{
				{Layers: []string{"/o2"}, Name: "left2", Priority: 50},
				{Layers: []string{"/o1"}, Name: "left1", Priority: 10},
			},
			expected: "lowerdir=/o1:/o2:/base",
		},
		{
			name:       "equal priority tie-break by name",
			baseLayers: []string{"/base"},
			leftExtensions: []Extension{
				{Layers: []string{"/zz"}, Name: "zulu", Priority: 10},
				{Layers: []string{"/aa"}, Name: "alpha", Priority: 10},
			},
			expected: "lowerdir=/aa:/zz:/base",
		},
		{
			name:       "both sides",
			baseLayers: []string{"/base"},
			leftExtensions: []Extension{
				{Layers: []string{"/o1"}, Name: "left1", Priority: 10},
			},
			rightExtensions: []Extension{
				{Layers: []string{"/n1"}, Name: "right1"},
				{Layers: []string{"/n2"}, Name: "right2"},
			},
			expected: "lowerdir=/o1:/base:/n1:/n2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildOverlayOptions(tt.baseLayers, tt.leftExtensions, tt.rightExtensions)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestBuildOverlayOptionsTruncation verifies page-size behavior: extensions
// are dropped as WHOLE chains (a partial chain would compose a partial
// image), rights before lefts, and the base chain is always present.
func TestBuildOverlayOptionsTruncation(t *testing.T) {
	pageSize := os.Getpagesize()

	baseLayers := []string{"/" + strings.Repeat("b", pageSize/3)}

	left := Extension{
		Layers:   []string{"/" + strings.Repeat("o", pageSize/3)},
		Name:     "left",
		Priority: 10,
	}
	rightLong := Extension{
		Layers: []string{"/" + strings.Repeat("n", pageSize/3)},
		Name:   "right",
	}

	result := BuildOverlayOptions(baseLayers, []Extension{left}, []Extension{rightLong})

	if !strings.Contains(result, left.Layers[0]) {
		t.Error("left extension chain missing from result")
	}
	if !strings.Contains(result, baseLayers[0]) {
		t.Error("base chain missing from result")
	}
	if strings.Contains(result, rightLong.Layers[0]) {
		t.Error("right extension should have been dropped due to page size limit")
	}
	if len(result) >= pageSize-1 {
		t.Errorf("result length %d exceeds page size limit %d", len(result), pageSize-1)
	}

	// A left extension whose chain does not fit is dropped whole, base stays.
	hugeLeft := Extension{
		Layers:   []string{"/" + strings.Repeat("x", pageSize)},
		Name:     "huge",
		Priority: 1,
	}
	degraded := BuildOverlayOptions([]string{"/base"}, []Extension{hugeLeft}, nil)
	if degraded != "lowerdir=/base" {
		t.Errorf("expected bare base chain, got %q", degraded)
	}

	// Whole-chain semantics: an extension with two layers where only the
	// first would fit must contribute NEITHER layer.
	half := pageSize / 2
	twoLayer := Extension{
		Layers:   []string{"/" + strings.Repeat("p", half/4), "/" + strings.Repeat("q", half)},
		Name:     "two-layer",
		Priority: 1,
	}
	partial := BuildOverlayOptions([]string{"/" + strings.Repeat("z", half)}, []Extension{twoLayer}, nil)
	if strings.Contains(partial, twoLayer.Layers[0]) || strings.Contains(partial, twoLayer.Layers[1]) {
		t.Errorf("partially-fitting chain must be dropped whole, got %q", partial)
	}

	// Rights are dropped before lefts.
	bigLeft := Extension{
		Layers:   []string{"/" + strings.Repeat("o", pageSize/2)},
		Name:     "big-left",
		Priority: 1,
	}
	mediumBase := []string{"/" + strings.Repeat("b", pageSize/4)}
	rightExt := Extension{
		Layers: []string{"/" + strings.Repeat("n", pageSize/4)},
		Name:   "right",
	}
	mixed := BuildOverlayOptions(mediumBase, []Extension{bigLeft}, []Extension{rightExt})
	if !strings.Contains(mixed, bigLeft.Layers[0]) {
		t.Error("left extension should be present")
	}
	if !strings.Contains(mixed, mediumBase[0]) {
		t.Error("base chain must always be present")
	}
	if strings.Contains(mixed, rightExt.Layers[0]) {
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
		rightExtensions[i] = Extension{Name: c.Name, Layers: c.Layers}
		t.Logf("OS block %d: %s at %s", i, c.Name, c.MountPath)
	}

	opts := BuildOverlayOptions(hostappContainers[0].Layers, nil, rightExtensions)
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

// buildAgnosticContainer builds an extension fixture that carries no
// /lib/modules/<release> tree.
func buildAgnosticContainer(t *testing.T, name string) Container {
	t.Helper()
	c := writeConfigV2(t, name, nil, nil)
	c.MountPath = t.TempDir()
	return c
}

// buildKernelImageContainer builds a kernel-claiming extension fixture for
// the image-hash identity: modules tree for release, a kernel image under
// /boot, and the kernel-abi-id label.
func buildKernelImageContainer(t *testing.T, name, release string, imageContent []byte, label string) Container {
	t.Helper()
	var labels map[string]string
	if label != "" {
		labels = map[string]string{HOSTOS_BLOCKS_KERNEL_ABI_ID: label}
	}
	c := writeConfigV2(t, name, labels, nil)

	mount := t.TempDir()
	modDir := filepath.Join(mount, "lib", "modules", release)
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "Module.symvers"), []byte("shared-symvers-same-config\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c.MountPath = mount

	if imageContent != nil {
		bootDir := filepath.Join(mount, "boot")
		if err := os.MkdirAll(bootDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bootDir, "Image.gz"), imageContent, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// imageIDOf returns the hex sha256 of content, as ResolveExtensionABIID
// computes it for a /boot file.
func imageIDOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func TestResolveExtensionABIID(t *testing.T) {
	const release = "6.1.0-test"
	img := []byte("kernel-image-build-X\n")
	id := imageIDOf(img)

	t.Run("agnostic: no modules tree, no claim", func(t *testing.T) {
		c := buildAgnosticContainer(t, "agnostic")
		got, err := c.ResolveExtensionABIID(release, id)
		if err != nil || got != "" {
			t.Errorf("want \"\", nil; got %q, %v", got, err)
		}
	})

	t.Run("empty mount path: no claim", func(t *testing.T) {
		c := Container{}
		got, err := c.ResolveExtensionABIID(release, id)
		if err != nil || got != "" {
			t.Errorf("want \"\", nil; got %q, %v", got, err)
		}
	})

	// The release=="" guard sits after the modules-tree stat, so an unknown
	// running kernel is fatal only for extensions that actually claim one.
	t.Run("empty release on an agnostic extension passes (no claim)", func(t *testing.T) {
		c := buildAgnosticContainer(t, "agnostic-unknown-release")
		got, err := c.ResolveExtensionABIID("", id)
		if err != nil || got != "" {
			t.Errorf("want \"\", nil; got %q, %v", got, err)
		}
	})

	t.Run("empty release on a module-carrying extension is an error (fail-closed)", func(t *testing.T) {
		c := buildKernelImageContainer(t, "kernel-unknown-release", release, img, id)
		if _, err := c.ResolveExtensionABIID("", id); err == nil {
			t.Error("expected error when running kernel release is unknown")
		}
	})

	t.Run("image matched against running kernel", func(t *testing.T) {
		c := buildKernelImageContainer(t, "km", release, img, id)

		cfgPath := filepath.Join(c.HomePath, "config.v2.json")
		before, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}

		got, err := c.ResolveExtensionABIID(release, id)
		if err != nil {
			t.Fatal(err)
		}
		if got != id {
			t.Errorf("got %q, want %q", got, id)
		}

		after, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("config.v2.json contents changed during early boot")
		}
	})

	t.Run("image found among other /boot entries", func(t *testing.T) {
		c := buildKernelImageContainer(t, "multi-boot", release, img, id)
		bootDir := filepath.Join(c.MountPath, "boot")
		// "Config-6.1.0" sorts before "Image.gz", so ReadDir yields the decoy
		// first: a resolver that only hashed the first entry would fail here.
		if err := os.WriteFile(filepath.Join(bootDir, "Config-6.1.0"), []byte("not-a-kernel\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(bootDir, "extlinux"), 0755); err != nil {
			t.Fatal(err)
		}

		got, err := c.ResolveExtensionABIID(release, id)
		if err != nil {
			t.Fatal(err)
		}
		if got != id {
			t.Errorf("got %q, want %q", got, id)
		}
	})

	// The label is advisory: a stale or missing label must not drop an
	// overlay whose shipped kernel IS the running kernel.
	t.Run("stale label but image matches running kernel: mounts", func(t *testing.T) {
		c := buildKernelImageContainer(t, "stale-label", release, img, imageIDOf([]byte("old-symvers-scheme\n")))
		got, err := c.ResolveExtensionABIID(release, id)
		if err != nil {
			t.Fatal(err)
		}
		if got != id {
			t.Errorf("got %q, want %q", got, id)
		}
	})

	t.Run("missing label but image matches running kernel: mounts", func(t *testing.T) {
		c := buildKernelImageContainer(t, "no-label", release, img, "")
		got, err := c.ResolveExtensionABIID(release, id)
		if err != nil {
			t.Fatal(err)
		}
		if got != id {
			t.Errorf("got %q, want %q", got, id)
		}
	})

	t.Run("modules without /boot image: broken", func(t *testing.T) {
		c := buildKernelImageContainer(t, "no-image", release, nil, id)
		if _, err := c.ResolveExtensionABIID(release, id); err == nil {
			t.Error("expected error for missing kernel image")
		}
	})

	t.Run("image does not match running kernel: skipped", func(t *testing.T) {
		c := buildKernelImageContainer(t, "mismatch", release, img, id)
		if _, err := c.ResolveExtensionABIID(release, imageIDOf([]byte("other-build\n"))); err == nil {
			t.Error("expected error when no /boot image hashes to hostABIID")
		}
	})

	t.Run("empty hostABIID (stock boot): kernel-carrying skipped", func(t *testing.T) {
		c := buildKernelImageContainer(t, "stock-boot", release, img, id)
		if _, err := c.ResolveExtensionABIID(release, ""); err == nil {
			t.Error("expected error for kernel-carrying extension on a stock-kernel boot")
		}
	})
}

func TestFilterByKernelABIID(t *testing.T) {
	const release = "6.1.0-test"

	imgA := []byte("kernel-image-build-A\n")
	imgB := []byte("kernel-image-build-B\n")
	hostA := imageIDOf(imgA)

	kmA := buildKernelImageContainer(t, "match", release, imgA, imageIDOf(imgA))
	kmB := buildKernelImageContainer(t, "wrong-build", release, imgB, imageIDOf(imgB))
	unlabeled := buildKernelImageContainer(t, "unlabeled", release, imgA, "")
	staleLabel := buildKernelImageContainer(t, "stale-label", release, imgA, imageIDOf([]byte("symvers-hash\n")))
	agnostic := buildAgnosticContainer(t, "agnostic")

	tests := []struct {
		name        string
		containers  []Container
		hostABIID   string
		expectNames []string
	}{
		{"agnostic passes", []Container{agnostic}, hostA, []string{"agnostic"}},
		{"matching build mounts", []Container{kmA}, hostA, []string{"match"}},
		{"other build skipped (same config, different image)", []Container{kmB}, hostA, nil},
		{"unlabeled but matching image mounts (label is advisory)", []Container{unlabeled}, hostA, []string{"unlabeled"}},
		{"stale label but matching image mounts (label is advisory)", []Container{staleLabel}, hostA, []string{"stale-label"}},
		{"absent host id skips kernel blocks, keeps agnostic", []Container{kmA, agnostic}, "", []string{"agnostic"}},
		{"mixed: only the booted build and agnostic pass", []Container{kmA, kmB, agnostic}, hostA, []string{"match", "agnostic"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FilterByKernelABIID(tt.containers, release, tt.hostABIID)
			var gotNames []string
			for _, c := range result {
				gotNames = append(gotNames, c.Name)
			}
			if !reflect.DeepEqual(gotNames, tt.expectNames) {
				t.Errorf("got %v, want %v", gotNames, tt.expectNames)
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
		{Config: Config{ID: "keepid", Name: "keep"}, MountPath: keepPath, Layers: []string{"/k1", "/k2"}},
		{Config: Config{
			ID:         "dropid",
			Name:       "drop",
			HostConfig: HostConfig{Labels: map[string]string{HOSTOS_BLOCKS_KERNEL_VERSION: "0.0.0"}},
		}, MountPath: dropPath, Layers: []string{"/d1"}},
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
	// Layers is chain metadata, not mount state: it must survive the unmount
	// so the flat root compose can reference chains of released scaffolding.
	if !reflect.DeepEqual(all[1].Layers, []string{"/d1"}) {
		t.Errorf("dropped container lost its Layers: %v", all[1].Layers)
	}
}

// fakeTwoLayerImage builds a two-layer fake overlay2 image (top + parent,
// lower file wired) under overlay2Dir and returns its top layer dir. prefix
// is a single distinguishing character. Overlayfs lower-only mounts need at
// least two lowerdirs, which is also why real container mounts always have
// the container top layer plus at least one image layer.
func fakeTwoLayerImage(t *testing.T, overlay2Dir, prefix string, files map[string]string) string {
	t.Helper()
	topID := strings.Repeat(prefix, 64)[:64]
	parentID := strings.Repeat(prefix+"0", 32)[:64]
	topShort := (strings.ToUpper(prefix) + strings.Repeat("T", 26))[:26]
	parentShort := (strings.ToUpper(prefix) + strings.Repeat("P", 26))[:26]
	fakeOverlay2Layer(t, overlay2Dir, topID, topShort)
	fakeOverlay2Layer(t, overlay2Dir, parentID, parentShort)
	topLayerDir := filepath.Join(overlay2Dir, topID)
	if err := os.WriteFile(filepath.Join(topLayerDir, "lower"), []byte("l/"+parentShort), 0644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(topLayerDir, "diff", name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return topLayerDir
}

// TestFlatComposeDepthBudget proves the flat root compose is a depth-1
// overlay: a second overlay can stack on its merged view.
func TestFlatComposeDepthBudget(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to perform overlay mounts")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("unshare: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("make mounts private: %v", err)
	}

	// Anchor every working dir on a tmpfs this test mounts itself (guaranteed depth 0).
	tmpfs := tmpfsRoot(t)
	tdir := func(name string) string {
		p := filepath.Join(tmpfs, name)
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	baseStore := tdir("base-store")
	baseTop := fakeTwoLayerImage(t, baseStore, "d", map[string]string{"base.txt": "from-base"})
	extStore := tdir("ext-store")
	extTop := fakeTwoLayerImage(t, extStore, "e", map[string]string{"base.txt": "from-ext"})

	baseLayers, err := buildLowerDirs(baseStore, baseTop)
	if err != nil {
		t.Fatalf("base buildLowerDirs: %v", err)
	}
	extLayers, err := buildLowerDirs(extStore, extTop)
	if err != nil {
		t.Fatalf("ext buildLowerDirs: %v", err)
	}

	root := tdir("root")
	opts := BuildOverlayOptions(baseLayers, []Extension{{Name: "ext", Layers: extLayers, Priority: 0}}, nil)
	if err := unix.Mount("overlay", root, "overlay", 0, opts); err != nil {
		t.Fatalf("flat root mount failed: %v (opts %q)", err, opts)
	}
	defer unix.Unmount(root, unix.MNT_DETACH)

	// Left extension shadows the base file.
	got, err := os.ReadFile(filepath.Join(root, "base.txt"))
	if err != nil {
		t.Fatalf("reading merged base.txt: %v", err)
	}
	if string(got) != "from-ext" {
		t.Errorf("base.txt = %q, want %q (override precedence)", got, "from-ext")
	}

	// The sealing pattern: one more overlay whose lowerdir sits on the root.
	seal := tdir("seal")
	dummy := tdir("dummy")
	sealOpts := "lowerdir=" + root + ":" + dummy
	if err := unix.Mount("overlay", seal, "overlay", 0, sealOpts); err != nil {
		t.Fatalf("depth-2 stack on flat root rejected: %v (this is the regression the flat compose fixes)", err)
	}
	unix.Unmount(seal, unix.MNT_DETACH)

	// Compose the two merged views (each an overlay) into a depth-2 root so
        // the sealing overlay on top then needs depth 3 and must be rejected.
	baseMerged := tdir("base-merged")
	if err := unix.Mount("overlay", baseMerged, "overlay", 0, "lowerdir="+strings.Join(baseLayers, ":")); err != nil {
		t.Fatalf("base merged mount: %v", err)
	}
	defer unix.Unmount(baseMerged, unix.MNT_DETACH)
	extMerged := tdir("ext-merged")
	if err := unix.Mount("overlay", extMerged, "overlay", 0, "lowerdir="+strings.Join(extLayers, ":")); err != nil {
		t.Fatalf("ext merged mount: %v", err)
	}
	defer unix.Unmount(extMerged, unix.MNT_DETACH)

	deep := tdir("deep")
	if err := unix.Mount("overlay", deep, "overlay", 0, "lowerdir="+extMerged+":"+baseMerged); err != nil {
		t.Fatalf("depth-2 merged-compose mount: %v", err)
	}
	defer unix.Unmount(deep, unix.MNT_DETACH)

	seal2 := tdir("seal2")
	if err := unix.Mount("overlay", seal2, "overlay", 0, "lowerdir="+deep+":"+dummy); err == nil {
		unix.Unmount(seal2, unix.MNT_DETACH)
		t.Fatal("depth-3 stack unexpectedly accepted; expected stacking-depth rejection")
	}
}

// tmpfsRoot mounts a fresh tmpfs in a temp dir and returns its path, so
// callers can place overlay layer dirs and mountpoints on a guaranteed
// depth-0 filesystem regardless of what backs the ambient temp dir (a DinD
// container's own overlay rootfs would otherwise silently add a stacking
// level). Requires an unshared, private mount namespace. The mount is torn
// down at test cleanup.
func tmpfsRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, ""); err != nil {
		t.Fatalf("mount tmpfs on %s: %v", dir, err)
	}
	t.Cleanup(func() { unix.Unmount(dir, unix.MNT_DETACH) })
	return dir
}

// opaqueMechanism probes which overlay opaque-dir xattr the running
// environment permits and returns the xattr name plus any extra mount option
// the flat-root overlay needs to honor it. Real root uses
// trusted.overlay.opaque with no extra option; a rootless user namespace is
// denied trusted.* and must use user.overlay.opaque together with the
// userxattr mount option.
func opaqueMechanism(t *testing.T) (xattr, mountOpt string) {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.Mkdir(probe, 0755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(probe, "trusted.overlay.opaque", []byte("y"), 0); err == nil {
		return "trusted.overlay.opaque", ""
	}
	if err := unix.Setxattr(probe, "user.overlay.opaque", []byte("y"), 0); err != nil {
		t.Skipf("no usable overlay opaque xattr in this environment: %v", err)
	}
	return "user.overlay.opaque", "userxattr"
}

// TestFlatComposeWhiteoutSemantics documents the ACCEPTED semantic change of
// the flat compose: a whiteout or opaque dir inside an extension layer hides
// the matching hostapp entry.
func TestFlatComposeWhiteoutSemantics(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root for mknod/setxattr/overlay mounts")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("unshare: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("make mounts private: %v", err)
	}

	opaqueXattr, mountOpt := opaqueMechanism(t)

	baseStore := t.TempDir()
	baseTop := fakeTwoLayerImage(t, baseStore, "d", map[string]string{
		"keep.conf":    "kept",
		"removed.conf": "should-vanish",
	})
	baseDiff := filepath.Join(baseTop, "diff")
	if err := os.MkdirAll(filepath.Join(baseDiff, "opq"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDiff, "opq", "from-base"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	extStore := t.TempDir()
	extTop := fakeTwoLayerImage(t, extStore, "e", nil)
	extDiff := filepath.Join(extTop, "diff")
	// The test runs fully under unshare -rm where mknod is permitted.
	if err := unix.Mknod(filepath.Join(extDiff, "removed.conf"), unix.S_IFCHR|0000, 0); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skipf("mknod denied in this environment (%v); needs unshare -rm or CAP_MKNOD", err)
		}
		t.Fatalf("mknod whiteout: %v", err)
	}
	// Opaque dir: shadows the base dir's whole content.
	if err := os.MkdirAll(filepath.Join(extDiff, "opq"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filepath.Join(extDiff, "opq"), opaqueXattr, []byte("y"), 0); err != nil {
		t.Fatalf("setxattr %s: %v", opaqueXattr, err)
	}
	if err := os.WriteFile(filepath.Join(extDiff, "opq", "from-ext"), []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}

	baseLayers, err := buildLowerDirs(baseStore, baseTop)
	if err != nil {
		t.Fatal(err)
	}
	extLayers, err := buildLowerDirs(extStore, extTop)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	opts := BuildOverlayOptions(baseLayers, []Extension{{Name: "ext", Layers: extLayers, Priority: 0}}, nil)
	if mountOpt != "" {
		opts += "," + mountOpt
	}
	if err := unix.Mount("overlay", root, "overlay", 0, opts); err != nil {
		t.Fatalf("flat root mount failed: %v (opts %q)", err, opts)
	}
	defer unix.Unmount(root, unix.MNT_DETACH)

	if _, err := os.ReadFile(filepath.Join(root, "keep.conf")); err != nil {
		t.Errorf("keep.conf must survive: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "removed.conf")); !os.IsNotExist(err) {
		t.Errorf("extension whiteout must hide base removed.conf (err=%v)", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "opq", "from-base")); !os.IsNotExist(err) {
		t.Errorf("opaque ext dir must hide base dir content (err=%v)", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "opq", "from-ext")); err != nil {
		t.Errorf("opaque ext dir's own content must be visible: %v", err)
	}
}

// TestBuildOverlayOptionsDedupSharedBase verifies the flat compose lists a
// layer shared between images exactly once, keeping the LAST occurrence.
func TestBuildOverlayOptionsDedupSharedBase(t *testing.T) {
	tests := []struct {
		name            string
		baseLayers      []string
		leftExtensions  []Extension
		rightExtensions []Extension
		expected        string
	}{
		{
			name:       "left extension shares the base image's bottom layer",
			baseLayers: []string{"/h1", "/shared"},
			leftExtensions: []Extension{
				{Layers: []string{"/e1", "/shared"}, Name: "ext", Priority: 0},
			},
			expected: "lowerdir=/e1:/h1:/shared",
		},
		{
			name:       "right extension shares the base image's bottom layer",
			baseLayers: []string{"/h1", "/shared"},
			rightExtensions: []Extension{
				{Layers: []string{"/r1", "/shared"}, Name: "right"},
			},
			expected: "lowerdir=/h1:/r1:/shared",
		},
		{
			name:       "two lefts share a two-layer suffix",
			baseLayers: []string{"/h1"},
			leftExtensions: []Extension{
				{Layers: []string{"/e1", "/s1", "/s2"}, Name: "e1", Priority: 0},
				{Layers: []string{"/e2", "/s1", "/s2"}, Name: "e2", Priority: 1},
			},
			expected: "lowerdir=/e1:/e2:/s1:/s2:/h1",
		},
		{
			name:       "all images share the base, order preserved",
			baseLayers: []string{"/h1", "/shared"},
			leftExtensions: []Extension{
				{Layers: []string{"/e1", "/shared"}, Name: "ext", Priority: 0},
			},
			rightExtensions: []Extension{
				{Layers: []string{"/r1", "/shared"}, Name: "right"},
			},
			expected: "lowerdir=/e1:/h1:/r1:/shared",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildOverlayOptions(tt.baseLayers, tt.leftExtensions, tt.rightExtensions)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestFlatComposeDedupSharedBaseKernel proves against the kernel that a base
// layer shared by the hostapp and an override extension mounts once.
func TestFlatComposeDedupSharedBaseKernel(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to perform overlay mounts")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("unshare: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("make mounts private: %v", err)
	}

	store := t.TempDir()

	sharedID := strings.Repeat("f", 64)
	sharedShort := "SHAREDBASEFFFFFFFFFFFFFFFF"
	fakeOverlay2Layer(t, store, sharedID, sharedShort)
	sharedDiff := filepath.Join(store, sharedID, "diff")
	for name, content := range map[string]string{"conf": "base", "shared-only": "x"} {
		if err := os.WriteFile(filepath.Join(sharedDiff, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	hostID := strings.Repeat("d", 64)
	hostShort := "HOSTTOPDDDDDDDDDDDDDDDDDDD"[:26]
	fakeOverlay2Layer(t, store, hostID, hostShort)
	hostDir := filepath.Join(store, hostID)
	if err := os.WriteFile(filepath.Join(hostDir, "lower"), []byte("l/"+sharedShort), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "diff", "conf"), []byte("host"), 0644); err != nil {
		t.Fatal(err)
	}

	extID := strings.Repeat("e", 64)
	extShort := "EXTTOPEEEEEEEEEEEEEEEEEEEE"[:26]
	fakeOverlay2Layer(t, store, extID, extShort)
	extDir := filepath.Join(store, extID)
	if err := os.WriteFile(filepath.Join(extDir, "lower"), []byte("l/"+sharedShort), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "diff", "ext-only"), []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}

	baseLayers, err := buildLowerDirs(store, hostDir)
	if err != nil {
		t.Fatalf("hostapp buildLowerDirs: %v", err)
	}
	extLayers, err := buildLowerDirs(store, extDir)
	if err != nil {
		t.Fatalf("ext buildLowerDirs: %v", err)
	}

	opts := BuildOverlayOptions(baseLayers, []Extension{{Name: "ext", Layers: extLayers, Priority: 0}}, nil)
	if n := strings.Count(opts, sharedShort); n != 1 {
		t.Fatalf("shared layer listed %d times in %q, want exactly once", n, opts)
	}

	root := t.TempDir()
	if err := unix.Mount("overlay", root, "overlay", 0, opts); err != nil {
		t.Fatalf("flat root mount with shared base failed: %v (opts %q)", err, opts)
	}
	defer unix.Unmount(root, unix.MNT_DETACH)

	got, err := os.ReadFile(filepath.Join(root, "conf"))
	if err != nil {
		t.Fatalf("reading merged conf: %v", err)
	}
	if string(got) != "host" {
		t.Errorf("conf = %q, want %q: hostapp's own override must win over the shared base", got, "host")
	}
	for _, f := range []string{"shared-only", "ext-only"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("%s missing from merged root: %v", f, err)
		}
	}
}

// storeEntry describes one container record in a synthetic engine store.
type storeEntry struct {
	dir               string
	id                string
	name              string
	class             string // class label value; "" omits the label
	abi               string // kernel-abi-id label value; "" omits the label
	dead              bool
	removalInProgress bool
}

// writeStore lays out <root>/containers/<dir>/config.v2.json for each entry
// and returns the docker data root, the argument readContainers takes.
func writeStore(t *testing.T, entries ...storeEntry) string {
	t.Helper()
	root := t.TempDir()
	for _, e := range entries {
		labels := map[string]interface{}{}
		if e.class != "" {
			labels[HOSTOS_BLOCKS_CLASS] = e.class
		}
		if e.abi != "" {
			labels[HOSTOS_BLOCKS_KERNEL_ABI_ID] = e.abi
		}
		record := map[string]interface{}{
			"ID":     e.id,
			"Name":   e.name,
			"Driver": "overlay2",
			"Config": map[string]interface{}{"Labels": labels},
			"State": map[string]interface{}{
				"Dead":              e.dead,
				"RemovalInProgress": e.removalInProgress,
			},
		}
		home := filepath.Join(root, "containers", e.dir)
		if err := os.MkdirAll(home, 0755); err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("marshal %s: %v", e.dir, err)
		}
		if err := os.WriteFile(filepath.Join(home, "config.v2.json"), out, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestReadContainers(t *testing.T) {
	t.Run("selects extensions by class label", func(t *testing.T) {
		root := writeStore(t,
			storeEntry{dir: "01", id: "aaa1", name: "ext", class: "overlay"},
			storeEntry{dir: "02", id: "bbb2", name: "app"},
			storeEntry{dir: "03", id: "ccc3", name: "other-class", class: "service"},
		)
		got, err := readContainers(root, HOSTOS_BLOCKS_CLASS)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "ext" {
			t.Fatalf("got %v, want just the overlay-classed container", names(got))
		}
	})

	// mountSysroot matches the hostapp by ID prefix, so the extraction has to
	// keep that arm as well as the label one.
	t.Run("selects the hostapp by ID prefix", func(t *testing.T) {
		root := writeStore(t,
			storeEntry{dir: "01", id: "deadbeefcafe", name: "hostapp"},
			storeEntry{dir: "02", id: "0badc0de", name: "ext", class: "overlay"},
		)
		got, err := readContainers(root, "deadbeef")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "hostapp" {
			t.Fatalf("got %v, want just the hostapp", names(got))
		}
	})

	t.Run("drops dead and removal-in-progress containers", func(t *testing.T) {
		root := writeStore(t,
			storeEntry{dir: "01", id: "aaa1", name: "live", class: "overlay"},
			storeEntry{dir: "02", id: "bbb2", name: "dead", class: "overlay", dead: true},
			storeEntry{dir: "03", id: "ccc3", name: "going", class: "overlay", removalInProgress: true},
		)
		got, err := readContainers(root, HOSTOS_BLOCKS_CLASS)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "live" {
			t.Fatalf("got %v, want just the live container", names(got))
		}
	})

	t.Run("skips an unreadable record without failing the walk", func(t *testing.T) {
		root := writeStore(t,
			storeEntry{dir: "01", id: "aaa1", name: "good", class: "overlay"},
			storeEntry{dir: "02", id: "bbb2", name: "corrupt", class: "overlay"},
		)
		bad := filepath.Join(root, "containers", "02", "config.v2.json")
		if err := os.WriteFile(bad, []byte("{not json"), 0644); err != nil {
			t.Fatal(err)
		}
		got, err := readContainers(root, HOSTOS_BLOCKS_CLASS)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "good" {
			t.Fatalf("got %v, want the readable container only", names(got))
		}
	})

	t.Run("ignores non-directory entries", func(t *testing.T) {
		root := writeStore(t, storeEntry{dir: "01", id: "aaa1", name: "ext", class: "overlay"})
		if err := os.WriteFile(filepath.Join(root, "containers", "stray"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		got, err := readContainers(root, HOSTOS_BLOCKS_CLASS)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("got %v, want one container", names(got))
		}
	})

	// Mount's contract is unchanged: a store it cannot read is an error, not
	// an empty result. Only ClaimedKernelABIs softens the missing-store case.
	t.Run("missing store is an error for the mount path", func(t *testing.T) {
		if _, err := initializeContainers(t.TempDir(), HOSTOS_BLOCKS_CLASS); err == nil {
			t.Fatal("expected an error for a store with no containers directory")
		}
	})
}

func names(containers []Container) []string {
	var out []string
	for _, c := range containers {
		out = append(out, c.Name)
	}
	return out
}

func TestClaimedKernelABIs(t *testing.T) {
	const abiA = "1111aaaa"
	const abiB = "2222bbbb"

	t.Run("reports the ABI of a live extension", func(t *testing.T) {
		root := writeStore(t, storeEntry{dir: "01", id: "aaa1", name: "km", class: "overlay", abi: abiA})
		got, err := ClaimedKernelABIs(root)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, []string{abiA}) {
			t.Errorf("got %v, want %v", got, []string{abiA})
		}
	})

	// The orphaned-override case: the arm survives in the bootenv, but nothing
	// in the store claims it any more.
	t.Run("a removed extension claims nothing", func(t *testing.T) {
		root := writeStore(t, storeEntry{dir: "01", id: "aaa1", name: "app"})
		got, err := ClaimedKernelABIs(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want no claims", got)
		}
	})

	// A dead container contributes no modules, because the mount path drops
	// it. Counting it as a claimant would retain exactly the arm that boots a
	// kernel with no drivers.
	t.Run("dead and removal-in-progress extensions do not claim", func(t *testing.T) {
		root := writeStore(t,
			storeEntry{dir: "01", id: "aaa1", name: "dead", class: "overlay", abi: abiA, dead: true},
			storeEntry{dir: "02", id: "bbb2", name: "going", class: "overlay", abi: abiB, removalInProgress: true},
		)
		got, err := ClaimedKernelABIs(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want no claims", got)
		}
	})

	t.Run("non-overlay containers do not claim", func(t *testing.T) {
		root := writeStore(t, storeEntry{dir: "01", id: "aaa1", name: "app", abi: abiA})
		got, err := ClaimedKernelABIs(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want no claims", got)
		}
	})

	t.Run("an ABI-agnostic extension does not claim", func(t *testing.T) {
		root := writeStore(t, storeEntry{dir: "01", id: "aaa1", name: "agnostic", class: "overlay"})
		got, err := ClaimedKernelABIs(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want no claims", got)
		}
	})

	t.Run("deduplicates and sorts", func(t *testing.T) {
		root := writeStore(t,
			storeEntry{dir: "01", id: "aaa1", name: "later", class: "overlay", abi: abiB},
			storeEntry{dir: "02", id: "bbb2", name: "earlier", class: "overlay", abi: abiA},
			storeEntry{dir: "03", id: "ccc3", name: "twin", class: "overlay", abi: abiB},
		)
		got, err := ClaimedKernelABIs(root)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, []string{abiA, abiB}) {
			t.Errorf("got %v, want %v", got, []string{abiA, abiB})
		}
	})

	// Unpopulated data root is unavailable, not an empty set.
	t.Run("missing containers directory is unavailable", func(t *testing.T) {
		got, err := ClaimedKernelABIs(t.TempDir())
		if !errors.Is(err, ErrClaimsUnavailable) {
			t.Fatalf("got %v, want ErrClaimsUnavailable", err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want no claims", got)
		}
	})

	// The only state answering with an empty claim set.
	t.Run("containers present with no claimant is an empty claim set", func(t *testing.T) {
		root := writeStore(t,
			storeEntry{dir: "01", id: "aaa1", name: "agnostic", class: "overlay"},
			storeEntry{dir: "02", id: "bbb2", name: "app"},
		)
		got, err := ClaimedKernelABIs(root)
		if err != nil {
			t.Fatalf("want a nil error for an empty claim set, got %v", err)
		}
		if got != nil {
			t.Errorf("got %v, want a nil claim set", got)
		}
	})

	// An absent data root means the partition is not mounted or the path is
	// wrong; reading it as a fresh store would silently drop every claim.
	t.Run("an absent data root is an error", func(t *testing.T) {
		if _, err := ClaimedKernelABIs(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Fatal("expected an error for an absent data root")
		}
	})

	// "cannot tell" must stay distinguishable from "nothing claims it": the
	// caller boots the override on the first and falls back to stock on the
	// second.
	t.Run("an unreadable store is an error", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "containers"), []byte("not a directory"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := ClaimedKernelABIs(root); err == nil {
			t.Fatal("expected an error when the containers path is not a directory")
		}
	})

	t.Run("an unsearchable store is an error", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		root := writeStore(t, storeEntry{dir: "01", id: "aaa1", name: "km", class: "overlay", abi: abiA})
		containers := filepath.Join(root, "containers")
		if err := os.Chmod(containers, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(containers, 0755)
		if _, err := ClaimedKernelABIs(root); err == nil {
			t.Fatal("expected an error for an unreadable containers directory")
		}
	})

	// The query runs from the bootloader initramfs against a read-only data
	// mount, so it must not create or modify anything.
	t.Run("does not write to the store", func(t *testing.T) {
		root := writeStore(t, storeEntry{dir: "01", id: "aaa1", name: "km", class: "overlay", abi: abiA})
		before := treeSnapshot(t, root)
		if _, err := ClaimedKernelABIs(root); err != nil {
			t.Fatal(err)
		}
		if after := treeSnapshot(t, root); !reflect.DeepEqual(before, after) {
			t.Errorf("store changed during the query:\nbefore %v\nafter  %v", before, after)
		}
	})
}

// treeSnapshot records every path under root with its size, enough to catch a
// query that creates, removes or rewrites anything.
func treeSnapshot(t *testing.T, root string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[rel] = info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}
