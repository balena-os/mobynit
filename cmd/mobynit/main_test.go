package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestGetMounts_RealMounts(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}

	// Create new mount namespace to isolate test mounts
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("failed to create mount namespace: %v", err)
	}

	// Make mounts private so changes don't propagate
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("failed to make mounts private: %v", err)
	}

	// Create and mount a tmpfs
	tmpDir := t.TempDir()
	if err := unix.Mount("tmpfs", tmpDir, "tmpfs", 0, ""); err != nil {
		t.Fatalf("failed to mount tmpfs: %v", err)
	}
	defer unix.Unmount(tmpDir, 0)

	mounts, err := getMounts()
	if err != nil {
		t.Fatalf("getMounts failed: %v", err)
	}

	found := false
	for _, mount := range mounts {
		if mount.Mountpoint == tmpDir {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("expected tmpfs mount at %s to appear in mounts list", tmpDir)
	}
}

func TestGetMounts_ParsesMultipleMounts(t *testing.T) {
	mounts, err := getMounts()
	if err != nil {
		t.Fatalf("getMounts failed: %v", err)
	}

	// Any system should have at least root and a few other mounts
	if len(mounts) < 2 {
		t.Errorf("expected at least 2 mounts, got %d", len(mounts))
	}
}

func TestGetMounts_NoDuplicateRoots(t *testing.T) {
	mounts, err := getMounts()
	if err != nil {
		t.Fatalf("getMounts failed: %v", err)
	}

	rootCount := 0
	for _, mount := range mounts {
		if mount.Mountpoint == "/" {
			rootCount++
		}
	}

	if rootCount > 1 {
		t.Errorf("root mount appeared %d times, expected 1", rootCount)
	}
}

func TestGetMounts_NestedMounts(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}

	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("failed to create mount namespace: %v", err)
	}

	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatalf("failed to make mounts private: %v", err)
	}

	// Create nested mount structure
	parentDir := t.TempDir()
	childDir := filepath.Join(parentDir, "child")
	if err := os.MkdirAll(childDir, 0755); err != nil {
		t.Fatalf("failed to create child dir: %v", err)
	}

	if err := unix.Mount("tmpfs", parentDir, "tmpfs", 0, ""); err != nil {
		t.Fatalf("failed to mount parent tmpfs: %v", err)
	}
	defer unix.Unmount(parentDir, unix.MNT_DETACH)

	// Recreate child dir after mounting parent
	if err := os.MkdirAll(childDir, 0755); err != nil {
		t.Fatalf("failed to create child dir after mount: %v", err)
	}

	if err := unix.Mount("tmpfs", childDir, "tmpfs", 0, ""); err != nil {
		t.Fatalf("failed to mount child tmpfs: %v", err)
	}
	defer unix.Unmount(childDir, unix.MNT_DETACH)

	mounts, err := getMounts()
	if err != nil {
		t.Fatalf("getMounts failed: %v", err)
	}

	parentFound, childFound := false, false
	for _, mount := range mounts {
		if mount.Mountpoint == parentDir {
			parentFound = true
		}
		if mount.Mountpoint == childDir {
			childFound = true
		}
	}

	if !parentFound {
		t.Error("expected parent mount to appear in mounts list")
	}
	if !childFound {
		t.Error("expected child mount to appear in mounts list")
	}
}

func TestGetMounts_ContainsStandardMounts(t *testing.T) {
	mounts, err := getMounts()
	if err != nil {
		t.Fatalf("getMounts failed: %v", err)
	}

	// Build set for quick lookup
	mountSet := make(map[string]bool)
	for _, m := range mounts {
		mountSet[m.Mountpoint] = true
	}

	// These should exist on any Linux system running tests
	standardMounts := []string{"/", "/proc", "/sys"}
	for _, expected := range standardMounts {
		if !mountSet[expected] {
			// Check if it might be a prefix match (sometimes paths are slightly different)
			found := false
			for m := range mountSet {
				if strings.HasPrefix(m, expected) || expected == m {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected standard mount %s not found", expected)
			}
		}
	}
}

// TestLowerdirFarm verifies the farm hands out short symlinks that resolve to
// their targets.
func TestLowerdirFarm(t *testing.T) {
	farm, err := newLowerdirFarm()
	if err != nil {
		t.Fatalf("newLowerdirFarm: %v", err)
	}
	defer os.RemoveAll(farm.dir)

	target1 := t.TempDir()
	target2 := t.TempDir()

	link1, err := farm.link(target1)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	link2, err := farm.link(target2)
	if err != nil {
		t.Fatalf("link: %v", err)
	}

	if link1 == link2 {
		t.Errorf("farm handed out the same path twice: %q", link1)
	}

	// Each symlink must resolve to its target.
	for _, tc := range []struct{ link, target string }{{link1, target1}, {link2, target2}} {
		got, err := filepath.EvalSymlinks(tc.link)
		if err != nil {
			t.Fatalf("resolving %s: %v", tc.link, err)
		}
		want, err := filepath.EvalSymlinks(tc.target)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s resolves to %q, want %q", tc.link, got, want)
		}
	}

	// The name must be a compact counter, not a 64-hex id: that is the point.
	if base := filepath.Base(link1); len(base) > 8 {
		t.Errorf("farm symlink name %q is not compact", base)
	}
}

// TestLowerdirFarmMountsUnderKernel proves the kernel follows the farm's
// absolute symlinks when they appear in a root overlay lowerdir, so extensions
// referenced compactly still stack. Runs only as (real or mapped) root.
func TestLowerdirFarmMountsUnderKernel(t *testing.T) {
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

	farm, err := newLowerdirFarm()
	if err != nil {
		t.Fatalf("newLowerdirFarm: %v", err)
	}
	defer os.RemoveAll(farm.dir)

	base := t.TempDir()
	ext := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "base.txt"), []byte("from-base"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ext, "ext.txt"), []byte("from-ext"), 0644); err != nil {
		t.Fatal(err)
	}

	baseLink, err := farm.link(base)
	if err != nil {
		t.Fatal(err)
	}
	extLink, err := farm.link(ext)
	if err != nil {
		t.Fatal(err)
	}

	mnt := t.TempDir()
	opts := "lowerdir=" + extLink + ":" + baseLink
	if err := unix.Mount("overlay", mnt, "overlay", 0, opts); err != nil {
		t.Fatalf("overlay mount rejected with farm symlink lowerdir %q: %v", opts, err)
	}
	defer unix.Unmount(mnt, unix.MNT_DETACH)

	for name, want := range map[string]string{"base.txt": "from-base", "ext.txt": "from-ext"} {
		got, err := os.ReadFile(filepath.Join(mnt, name))
		if err != nil {
			t.Errorf("reading %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestUnescapeMountpoint_NoEscape(t *testing.T) {
	input := "/mnt/data"
	result := unescapeMountpoint(input)
	if result != input {
		t.Errorf("expected %q, got %q", input, result)
	}
}

func TestUnescapeMountpoint_Space(t *testing.T) {
	// \040 is octal for space (32)
	input := "/mnt/my\\040data"
	expected := "/mnt/my data"
	result := unescapeMountpoint(input)
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestUnescapeMountpoint_Tab(t *testing.T) {
	// \011 is octal for tab (9)
	input := "/mnt/my\\011data"
	expected := "/mnt/my\tdata"
	result := unescapeMountpoint(input)
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestUnescapeMountpoint_Backslash(t *testing.T) {
	// \134 is octal for backslash (92)
	input := "/mnt/my\\134data"
	expected := "/mnt/my\\data"
	result := unescapeMountpoint(input)
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestUnescapeMountpoint_Multiple(t *testing.T) {
	// Multiple escapes
	input := "/mnt/my\\040data\\040here"
	expected := "/mnt/my data here"
	result := unescapeMountpoint(input)
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestUnescapeMountpoint_InvalidOctal(t *testing.T) {
	// Invalid octal (not 3 digits) should be left as-is
	input := "/mnt/my\\04data"
	result := unescapeMountpoint(input)
	// Should preserve the backslash since it's not a valid 3-digit octal
	if result != input {
		t.Errorf("expected %q (unchanged), got %q", input, result)
	}
}

func TestUnescapeMountpoint_TrailingBackslash(t *testing.T) {
	// Backslash at end without enough chars
	input := "/mnt/data\\"
	result := unescapeMountpoint(input)
	if result != input {
		t.Errorf("expected %q (unchanged), got %q", input, result)
	}
}
