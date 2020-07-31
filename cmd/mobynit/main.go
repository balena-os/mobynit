/*
	Mobynit can either mount a custom sysroot if specified on the command
	line, or pivot root inside a default sysroot.
*/
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/docker/docker/pkg/mount"
	"golang.org/x/sys/unix"

	"github.com/balena-os/hostapp"
)

const (
	HOSTAPP_LAYER_ROOT    = "balena"
	PIVOT_PATH            = "/mnt/sysroot/active"
	FEATURE_HOSTAPP       = "io.balena.features.hostapp"
	FEATURE_HOSTEXTENSION = "io.balena.features.host-extension"
	LOG_DIR               = "/tmp/initramfs/"
	LOG_FILE              = "initramfs.debug"
	CMDLINE_NO_HOSTEXT    = "balena.nohostext"
	DATA_DIR_NAME         = "resin-data"
	DATA_LAYER_ROOT       = "docker"
)

/* Do not overlay host extensions*/
var nohostext bool

/* Filesystem type for data partition holdign host extensions */
var hostextFstype string

/* Hostapps contain a current symlink to the hostapp home directory
 * instead of being labelled. This allows for atomic hostapp updates
 * (just a rename on the symlink).
 * Mouting hostapps by label is implemented but not currently used as
 * image labels cannot be atomically updated.
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
	} else {
		containers, err = hostapp.Mount(layerRoot, FEATURE_HOSTAPP)
	}
	if len(containers) != 1 {
		return nil, fmt.Errorf("Invalid number of hostapp mounts: %d", len(containers))
	}
	return containers, err
}

func mountHostExtensions(newRootPath string) error {
	device, err := os.Readlink(filepath.Join("/dev/disk/by-state/", DATA_DIR_NAME))
	if err != nil {
		return fmt.Errorf("No udev by-state resin-data symbolic link")
	}
	// As the /dev mount was moved this cannot be used directly
	device = filepath.Join("/dev", string(os.PathSeparator), path.Base(device))
	err = unix.Mount(device, filepath.Join(newRootPath, string(os.PathSeparator), DATA_DIR_NAME), hostextFstype, 0, "")
	if err != nil {
		return fmt.Errorf("Error mounting data partition: %v", err)
	}

	containers, err := hostapp.Mount(filepath.Join(newRootPath, string(os.PathSeparator), filepath.Join(DATA_DIR_NAME, string(os.PathSeparator), DATA_LAYER_ROOT)), FEATURE_HOSTEXTENSION)
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
					mountOptions += filepath.Join(string(os.PathListSeparator), container.MountPath)
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
					return fmt.Errorf("Only overlay2 host extension mounts supported, not %v", container.Config.Driver)
				}
			}

			if err := unix.Mount(device, newRootPath, mountType, uintptr(0), mountOptions); err != nil {
				return fmt.Errorf("Error mounting host extension: %v", err)
			}

			log.Println("Overlayed host extensions:")
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

	if !nohostext {
		if err := mountHostExtensions(newRootPath); err != nil {
			log.Print(err)
		}
	}
	return newRootPath, nil
}

func main() {
	sysrootPtr := flag.String("sysroot", "", "root of partition e.g. /mnt/sysroot/inactive. Mount destination is returned in stdout")
	flag.StringVar(&hostextFstype, "dataFstype", "ext4", "Filesystem type for the data partition holding host extensions. Defaults to ext4.")
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
			if strings.Contains(arg, "emergency") || strings.Contains(arg, CMDLINE_NO_HOSTEXT) {
				nohostext = true
			}
		}
	}

	// Any mounts done by initrd will be transfered in the new root
	mounts, err := mount.GetMounts(nil)
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

	for _, mount := range mounts {
		if mount.Mountpoint == "/" {
			continue
		}
		if err := unix.Mount(mount.Mountpoint, filepath.Join(newRoot, mount.Mountpoint), "", unix.MS_MOVE, ""); err != nil {
			log.Println("could not move mountpoint:", mount.Mountpoint, err)
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
