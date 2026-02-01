/*
	Mobynit can either mount a custom sysroot if specified on the command
	line, or pivot root inside a default sysroot.
*/
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
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
	HOSTOS_BLOCKS_CLASS      = "io.balena.image.class"
	LOG_DIR                  = "/tmp/initramfs/"
	LOG_FILE                 = "initramfs.debug"
	CMDLINE_DISABLE_OVERLAYS = "balena.disable_overlays"
	CMDLINE_DEBUG_SHELL      = "mobynit.shell"
	DATA_DIR_NAME            = "/mnt/data"
	DATA_STATE_NAME          = "resin-data"
	DATA_LAYER_ROOT          = "docker"
)

/* Do not overlay images */
var disable_overlays bool

/* Drop to debug shell instead of exec init */
var debug_shell bool

/* Filesystem type for data partition */
var dataFstype string

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

func mountDataOverlays(newRootPath string) error {
	device, err := os.Readlink(filepath.Join("/dev/disk/by-state/", DATA_STATE_NAME))
	if err != nil {
		return fmt.Errorf("No udev by-state resin-data symbolic link")
	}
	// As the /dev mount was moved this cannot be used directly
	device = filepath.Join("/dev", string(os.PathSeparator), path.Base(device))
	err = unix.Mount(device, filepath.Join(newRootPath, string(os.PathSeparator), DATA_DIR_NAME), dataFstype, 0, "")
	if err != nil {
		return fmt.Errorf("Error mounting data partition: %v", err)
	}

	containers, err := hostapp.Mount(filepath.Join(newRootPath, string(os.PathSeparator), filepath.Join(DATA_DIR_NAME, string(os.PathSeparator), DATA_LAYER_ROOT)), HOSTOS_BLOCKS_CLASS)
	if err == nil {
		mountOptions := fmt.Sprintf("lowerdir=%s", newRootPath)
		mountType := "overlay"
		device := "overlay"
		var mountedContainers []string
		if len(containers) > 0 {
			for _, container := range containers {
				switch container.Config.Driver {
				case "overlay2":
					oldMountOptions := mountOptions
					mountOptions += ":" + container.MountPath
					// The kernel limits the mount option to PAGE_SIZE-1
					// https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/tree/fs/namespace.c?h=master#n3109
					if len(mountOptions) >= os.Getpagesize()-1 {
						mountOptions = oldMountOptions
						log.Println("Mount options too large - capping at page size")
						break
					}
					mountedContainers = append(mountedContainers, container.Config.Name)
				default:
					// aufs does not support nested mounts
					// https://sourceforge.net/p/aufs/mailman/message/31984065/
					return fmt.Errorf("Only overlay2 images supported, not %v", container.Config.Driver)
				}
			}

			if err := unix.Mount(device, newRootPath, mountType, uintptr(0), mountOptions); err != nil {
				return fmt.Errorf("Error mounting image: %v", err)
			}

			log.Println("Overlayed images:")
			for i, name := range mountedContainers {
				log.Printf("\t[%d] %s\n", i, name)
			}
		}
	}
	return err
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
		if err := mountDataOverlays(newRootPath); err != nil {
			log.Print(err)
		}
	}

	return newRootPath, nil
}

func main() {
	sysrootPtr := flag.String("sysroot", "", "root of partition e.g. /mnt/sysroot/inactive. Mount destination is returned in stdout")
	flag.StringVar(&dataFstype, "dataFstype", "ext4", "Filesystem type for the data partition. Defaults to ext4.")
	flag.Parse()

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

	content, err := ioutil.ReadFile("/proc/cmdline")
	if err == nil {
		args := strings.Fields(string(content))
		for _, arg := range args {
			if strings.Contains(arg, "emergency") || strings.Contains(arg, CMDLINE_DISABLE_OVERLAYS) {
				disable_overlays = true
			}
			if strings.Contains(arg, CMDLINE_DEBUG_SHELL) {
				debug_shell = true
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

	if debug_shell {
		log.Println("mobynit.shell: dropping to debug shell")
		if err := syscall.Exec("/bin/sh", []string{"/bin/sh"}, os.Environ()); err != nil {
			log.Fatalln("error executing shell:", err)
		}
	}

	if err := syscall.Exec("/sbin/init", os.Args, os.Environ()); err != nil {
		log.Fatalln("error executing init:", err)
	}
}
