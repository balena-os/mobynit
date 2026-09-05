/*
Mobynit can either mount a custom sysroot if specified on the command
line, or pivot root inside a default sysroot.
*/
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/balena-os/hostapp"
)

// MountInfo represents a mount point from /proc/self/mountinfo
type MountInfo struct {
	Mountpoint string
}

// getMounts parses /proc/self/mountinfo and returns mount points
func getMounts() ([]MountInfo, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var mounts []MountInfo
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// mountinfo format: ID PARENT_ID MAJOR:MINOR ROOT MOUNTPOINT OPTIONS...
		// Fields are space-separated, mountpoint is field 5 (index 4)
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		// Unescape octal sequences in mountpoint (e.g., \040 for space)
		mountpoint := unescapeMountpoint(fields[4])
		mounts = append(mounts, MountInfo{Mountpoint: mountpoint})
	}
	return mounts, scanner.Err()
}

// unescapeMountpoint handles octal escape sequences in mountinfo
// Escaped chars: space(\040), tab(\011), newline(\012), backslash(\134)
func unescapeMountpoint(s string) string {
	if strings.IndexByte(s, '\\') == -1 {
		return s
	}

	buf := make([]byte, len(s))
	bufLen := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+3 >= len(s) {
			buf[bufLen] = s[i]
			bufLen++
			continue
		}
		// Check for valid octal escape \NNN
		c1, c2, c3 := s[i+1], s[i+2], s[i+3]
		if c1 >= '0' && c1 <= '7' && c2 >= '0' && c2 <= '7' && c3 >= '0' && c3 <= '7' {
			v := (c1-'0')<<6 | (c2-'0')<<3 | (c3 - '0')
			buf[bufLen] = v
			bufLen++
			i += 3
		} else {
			buf[bufLen] = s[i]
			bufLen++
		}
	}
	return string(buf[:bufLen])
}

const (
	HOSTAPP_LAYER_ROOT       = "balena"
	PIVOT_PATH               = "/mnt/sysroot/active"
	LOG_DIR                  = "/tmp/initramfs/"
	LOG_FILE                 = "initramfs.debug"
	CMDLINE_DISABLE_OVERLAYS = "mobynit.no_overlays"
	DATA_WORK_DIR            = "/tmp/mobynit-data"
	DATA_STATE_NAME          = "resin-data"
	DATA_LAYER_ROOT          = "docker"
)

/* Do not overlay images */
var disable_overlays bool

// lowerdirFarm hands out short numbered symlinks pointing at extension merged
// mountpoints.
type lowerdirFarm struct {
	dir   string
	n     int
	links map[string]string
}

func newLowerdirFarm() (*lowerdirFarm, error) {
	dir, err := os.MkdirTemp("", "mobynit-lower")
	if err != nil {
		return nil, fmt.Errorf("creating lowerdir farm: %w", err)
	}
	return &lowerdirFarm{dir: dir, links: make(map[string]string)}, nil
}

// link creates a short absolute symlink to target and returns its path.
func (f *lowerdirFarm) link(target string) (string, error) {
	if name, ok := f.links[target]; ok {
		return name, nil
	}
	name := filepath.Join(f.dir, strconv.Itoa(f.n))
	if err := os.Symlink(target, name); err != nil {
		return "", fmt.Errorf("linking %s: %w", target, err)
	}
	f.n++
	f.links[target] = name
	return name, nil
}

// shortenChain replaces each layer reference with a compact farm symlink.
// A nil farm or a failed link degrades to the original path.
func shortenChain(farm *lowerdirFarm, name string, layers []string) []string {
	out := make([]string, 0, len(layers))
	for _, l := range layers {
		if farm == nil {
			out = append(out, l)
			continue
		}
		short, err := farm.link(l)
		if err != nil {
			log.Printf("Warning: compacting %s lowerdir failed: %v; using full path", name, err)
			out = append(out, l)
			continue
		}
		out = append(out, short)
	}
	return out
}

/* Filesystem type for data partition */
var dataFstype string

/* Where udev publishes the by-state partition symlinks. A variable so tests
 * can point the data partition lookup at a fixture. */
var stateDiskDir = "/dev/disk/by-state/"

/* Hostapps contain a current symlink to the hostapp home directory
 * instead of being labelled. This allows for atomic hostapp updates
 * (just a rename on the symlink).
 */
func mountSysroot(rootdir string) ([]hostapp.Container, error) {
	var containers []hostapp.Container
	current, err := os.Readlink(filepath.Join(rootdir, "current"))
	layerRoot := filepath.Join(rootdir, string(os.PathSeparator), HOSTAPP_LAYER_ROOT)
	if err == nil {
		cid := filepath.Base(current)
		containers, err = hostapp.Mount(layerRoot, cid)
		if err != nil {
			return nil, fmt.Errorf("Error mounting container with ID %s (len %d): %v", cid, len(containers), err)
		}
	}

	if len(containers) != 1 {
		return nil, fmt.Errorf("Invalid number of hostapp mounts: %d", len(containers))
	}
	return containers, err
}

func mountDataOverlays(newRootPath string, baseLayers []string) error {
	device, err := os.Readlink(filepath.Join(stateDiskDir, DATA_STATE_NAME))
	if err != nil {
		return fmt.Errorf("No udev by-state resin-data symbolic link")
	}
	// As the /dev mount was moved this cannot be used directly
	device = filepath.Join("/dev", string(os.PathSeparator), path.Base(device))

	// This is mobynit's own working mount, needed only to read the extension
	// layers under <data>/docker while the root is composed.
	// It is deliberately outside newRootPath, because the composed overlay is
	// stacked over newRootPath below and would shadow anything mounted beneath it.
	if err := os.MkdirAll(DATA_WORK_DIR, 0755); err != nil {
		return fmt.Errorf("Error creating %s: %v", DATA_WORK_DIR, err)
	}
	if err := unix.Mount(device, DATA_WORK_DIR, dataFstype, 0, ""); err != nil {
		return fmt.Errorf("Error mounting data partition: %v", err)
	}
	// Detached rather than unmounted so that the release cannot fail on an
	// error path that left an extension overlay mounted underneath. The
	// composed root keeps working either way as it references the extension
	// layers by dentry, which holds the superblock open once the mount is out
	// of the namespace the booted system inherits.
	defer func() {
		if err := unix.Unmount(DATA_WORK_DIR, unix.MNT_DETACH); err != nil {
			log.Printf("Warning: releasing data partition mount %s: %v", DATA_WORK_DIR, err)
		}
	}()

	// Check for pending purge - if remove_me_to_reset is missing,
	// data partition will be wiped after boot, so skip extension mounting. An
	// unreadable marker is not proof that the purge is disarmed.
	purgeMarker := filepath.Join(DATA_WORK_DIR, hostapp.PURGE_MARKER_FILE)
	if _, err := os.Stat(purgeMarker); err != nil {
		log.Printf("Purge pending: %s unreadable (%v), skipping extension overlays", hostapp.PURGE_MARKER_FILE, err)
		return nil
	}

	containers, err := hostapp.Mount(filepath.Join(DATA_WORK_DIR, DATA_LAYER_ROOT), hostapp.HOSTOS_BLOCKS_CLASS)
	if err != nil {
		return err
	}

	if len(containers) == 0 {
		return nil
	}

	// An empty release (e.g. uname failed) disables the version filter
	release, err := hostapp.GetKernelRelease()
	if err != nil {
		log.Printf("Warning: could not get kernel release: %v", err)
	}

	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		log.Printf("Warning: could not read /proc/cmdline: %v", err)
	}
	hostABIID := hostapp.ParseHostKernelABIID(string(cmdline))

	containers = hostapp.SelectMountable(containers, release, hostABIID)
	if len(containers) == 0 {
		log.Println("No extensions compatible with running kernel, skipping overlay")
		return nil
	}

	farm, err := newLowerdirFarm()
	if err != nil {
		log.Printf("Warning: %v; using full extension paths", err)
	}

	baseLayers = shortenChain(farm, "hostapp", baseLayers)

	var leftExtensions, rightExtensions []hostapp.Extension

	for _, container := range containers {
		if container.Config.Driver != "overlay2" {
			return fmt.Errorf("Only overlay2 images supported, not %v", container.Config.Driver)
		}
		ext := hostapp.Extension{
			Name:   container.Config.Name,
			Layers: shortenChain(farm, container.Config.Name, container.Layers),
		}
		if overrideVal, ok := container.Labels[hostapp.HOSTOS_BLOCKS_OVERRIDE]; ok {
			priority, err := strconv.Atoi(overrideVal)
			if err != nil {
				priority = math.MaxInt
				log.Printf("Warning: container %s has invalid override priority %q, defaulting to lowest", container.Config.Name, overrideVal)
			}
			ext.Priority = priority
			leftExtensions = append(leftExtensions, ext)
		} else {
			rightExtensions = append(rightExtensions, ext)
		}
	}

	for i := range containers {
		if err := containers[i].Unmount(); err != nil {
			log.Printf("Warning: releasing extension mount %s: %v", containers[i].Config.Name, err)
		}
	}

	mountOptions := hostapp.BuildOverlayOptions(baseLayers, leftExtensions, rightExtensions)

	if err := unix.Mount("overlay", newRootPath, "overlay", 0, mountOptions); err != nil {
		return fmt.Errorf("Error mounting image: %v", err)
	}

	return nil
}

func prepareForPivot() (string, error) {
	var newRootPath string
	if err := os.MkdirAll("/dev/shm", os.ModePerm); err != nil {
		return "", fmt.Errorf("Creating /dev/shm failed: %v", err)
	}

	if err := unix.Mount("shm", "/dev/shm", "tmpfs", 0, ""); err != nil {
		return "", fmt.Errorf("Error mounting /dev/shm: %v", err)
	}
	defer func() {
		if err := unix.Unmount("/dev/shm", unix.MNT_DETACH); err != nil {
			log.Println("error unmounting /dev/shm")
		}
	}()

	var containers []hostapp.Container
	containers, err := mountSysroot(string(os.PathSeparator))
	if err != nil {
		return "", fmt.Errorf("Error mounting sysroot: %v", err)
	}

	if len(containers) != 1 {
		return "", fmt.Errorf("No hostapp found: %d", len(containers))
	}

	newRootPath = containers[0].MountPath
	defer func() {
		if err := unix.Mount("", newRootPath, "", unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			log.Println("Error remounting new root as read-only:", err)
		}
	}()

	if err := os.MkdirAll(filepath.Join(newRootPath, PIVOT_PATH), os.ModePerm); err != nil {
		return newRootPath, fmt.Errorf("Creating %s failed: %v", PIVOT_PATH, err)
	}

	if !disable_overlays {
		if err := mountDataOverlays(newRootPath, containers[0].Layers); err != nil {
			log.Print(err)
		}
	}
	return newRootPath, nil
}

func main() {
	sysrootPtr := flag.String("sysroot", "", "root of partition e.g. /mnt/sysroot/inactive. Mount destination is returned in stdout")
	claimedPtr := flag.String("claimed-abis", "", "docker data root to report the kernel ABI ids whose modules this boot will mount, one per line, then exit")
	flag.StringVar(&dataFstype, "dataFstype", "ext4", "Filesystem type for the data partition. Defaults to ext4.")
	flag.Parse()

	// A read-only query, run from the bootloader initramfs before the kexec.
	// It must mount nothing and write nothing, so it returns ahead of the log
	// directory setup and of everything that reads /proc/cmdline. A flag given
	// with an empty value (a botched shell variable in the bootloader script)
	// must fail closed here rather than fall through into the pid-1 boot
	// sequence, which would mount and pivot inside the bootloader initramfs.
	claimedGiven := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "claimed-abis" {
			claimedGiven = true
		}
	})
	if claimedGiven {
		if *claimedPtr == "" {
			log.Fatalln("-claimed-abis requires a docker data root path")
		}
		abis, err := hostapp.ClaimedKernelABIs(*claimedPtr)
		// Cannot tell means claim nothing, as kexec expects
		if err != nil && !errors.Is(err, hostapp.ErrClaimsUnavailable) {
			log.Fatalln("Error reading claimed kernel ABIs:", err)
		}
		for _, abi := range abis {
			fmt.Println(abi)
		}
		return
	}

	if sysrootPtr != nil && *sysrootPtr != "" {
		var containers []hostapp.Container
		containers, err := mountSysroot(*sysrootPtr)
		if err != nil {
			log.Fatalln("Error mounting sysroot:", err)
		}
		fmt.Print(containers[0].MountPath)
		return
	}

	err := os.MkdirAll(LOG_DIR, 0777)
	if err == nil {
		lf, err := os.OpenFile(filepath.Join(LOG_DIR, LOG_FILE), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err == nil {
			defer lf.Close()
		}
		log.SetOutput(lf)
		log.SetPrefix("[init][INFO] ")
		// Omit timestamps as devices without RTC will see epoch
		log.SetFlags(log.Flags() &^ (log.Ldate | log.Ltime))
	}

	content, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		log.Printf("warning: could not read /proc/cmdline: %v (overlay flags ignored)", err)
	} else {
		args := strings.Fields(string(content))
		for _, arg := range args {
			if strings.Contains(arg, "emergency") || strings.Contains(arg, CMDLINE_DISABLE_OVERLAYS) {
				disable_overlays = true
			}
		}
	}

	// Any mounts done by initrd will be transfered in the new root
	mounts, err := getMounts()
	if err != nil {
		log.Fatalln("could not get mounts:", err)
	}

	if err := unix.Mount("", "/", "", unix.MS_REMOUNT, ""); err != nil {
		log.Fatalln("error remounting root as read/write:", err)
	}

	newRoot, err := prepareForPivot()
	if err != nil {
		log.Fatalln("Error preparing for pivot root:", err)
	}

	for _, m := range mounts {
		if m.Mountpoint == "/" {
			continue
		}
		if err := unix.Mount(m.Mountpoint, filepath.Join(newRoot, m.Mountpoint), "", unix.MS_MOVE, ""); err != nil {
			log.Println("could not move mountpoint:", m.Mountpoint, err)
		}
	}

	if err := syscall.PivotRoot(newRoot, filepath.Join(newRoot, PIVOT_PATH)); err != nil {
		log.Fatalln("error while pivoting root:", err)
	}

	if err := unix.Chdir("/"); err != nil {
		log.Fatal(err)
	}

	if err := syscall.Exec("/sbin/init", os.Args, os.Environ()); err != nil {
		log.Fatalln("error executing init:", err)
	}
}
