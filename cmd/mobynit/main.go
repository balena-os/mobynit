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
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dchest/uniuri"
	"github.com/docker/docker/pkg/mount"
	"github.com/opencontainers/selinux/go-selinux/label"
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

/* Legacy hostapps contain a current symlink to the hostapp home directory
 * instead of being labelled.
 */
func mountSysroot(rootdir string) ([]hostapp.Container, error) {
	var containers []hostapp.Container
	current, err := os.Readlink(filepath.Join(rootdir, "current"))
	layerRoot := filepath.Join(rootdir, string(os.PathSeparator), HOSTAPP_LAYER_ROOT)
	if err == nil {
		cid := filepath.Base(current)
		containers, err = hostapp.Mount(layerRoot, cid)
		if err != nil {
			return nil, fmt.Errorf("Error mounting container with ID %d (len %d): %v", cid, len(containers), err)
		}
	} else {
		containers, err = hostapp.Mount(layerRoot, FEATURE_HOSTAPP)
	}
	if len(containers) != 1 {
		return nil, fmt.Errorf("Invalid number of hostapp mounts: %d", len(containers))
	}
	return containers, err
}

func mountFrom(dir, device, target, mType, label string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	if err := unix.Mount(device, target, mType, uintptr(0), label); err != nil {
		return err
	}
	if err := os.Chdir(cwd); err != nil {
		return err
	}
	return nil
}

func mountHostExtensions(newRootPath string) error {
	device, err := os.Readlink(filepath.Join("/dev/disk/by-state/", DATA_DIR_NAME))
	if err != nil {
		return fmt.Errorf("No udev by-state resin-data symbolic link")
	}
	// As the /dev mount was moved this cannot be used directly
	device = filepath.Join("/dev", string(os.PathSeparator), path.Base(device))
	fstype, err := exec.Command(filepath.Join(newRootPath, "/usr/bin/lsblk"), "-no", "FSTYPE", device).Output()
	if err != nil {
		log.Print("Data partition file system type could not be detected, default to ext4: ", err)
		fstype = []byte("ext4")
	}
	err = unix.Mount(device, filepath.Join(newRootPath, string(os.PathSeparator), DATA_DIR_NAME), string(fstype), 0, "")
	if err != nil {
		return fmt.Errorf("Error mounting data partition: %v", err)
	}

	containers, err := hostapp.Mount(filepath.Join(newRootPath, string(os.PathSeparator), filepath.Join(DATA_DIR_NAME, string(os.PathSeparator), DATA_LAYER_ROOT)), FEATURE_HOSTEXTENSION)
	if err == nil {
		for _, container := range containers {
			var mountOptions string
			var mountType string
			switch container.Config.Driver {
			case "overlay2":
				mountOptions = fmt.Sprintf("lowerdir=%s", filepath.Join(newRootPath, string(os.PathListSeparator), container.MountPath))
				mountType = "overlay"
				break
			case "aufs":
				mountOptions = fmt.Sprintf("br=%s", filepath.Join(newRootPath, string(os.PathListSeparator), container.MountPath))
				mountType = "aufs"
				break
			default:
				return fmt.Errorf("Only overlay2/aufs host extension mounts supported, not %v", container.Config.Driver)
			}
			mountOptions = label.FormatMountLabel(mountOptions, uniuri.New())
			if len(mountOptions) > os.Getpagesize() {
				return fmt.Errorf("Mount options too large %d", len(mountOptions))
			}
			if err := mountFrom(container.MountPath, mountType, newRootPath, mountType, mountOptions); err != nil {
				return fmt.Errorf("Error mounting host extension: %v", err)
			}
			log.Printf("Host extension %s overlayed", container.Name)
		}
	} else {
		return fmt.Errorf("Error mounting host extensions: %v", err)
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
	defer unix.Unmount("/dev/shm", unix.MNT_DETACH)

	var containers []hostapp.Container
	containers, err := mountSysroot(string(os.PathSeparator))
	if err != nil {
		return "", fmt.Errorf("Error mounting sysroot: %v", err)
	}

	if len(containers) != 1 {
		return "", fmt.Errorf("No hostapp found: %d", len(containers))
	}

	newRootPath = containers[0].MountPath
	defer unix.Mount("", newRootPath, "", unix.MS_REMOUNT|unix.MS_RDONLY, "")

	if err := os.MkdirAll(filepath.Join(newRootPath, PIVOT_PATH), os.ModePerm); err != nil {
		return newRootPath, fmt.Errorf("Creating %s failed: %v", PIVOT_PATH, err)
	}

	if nohostext == false {
		if err := mountHostExtensions(newRootPath); err != nil {
			log.Print("Host extensions not mounted")
		}
	}
	return newRootPath, nil
}

func main() {
	sysrootPtr := flag.String("sysroot", "", "root of partition e.g. /mnt/sysroot/inactive. Mount destination is returned in stdout")
	flag.Parse()

	if sysrootPtr != nil && *sysrootPtr != "" {
		var containers []hostapp.Container
		containers, err := mountSysroot(*sysrootPtr)
		if err != nil {
			log.Fatal("Error mounting sysroot:", err)
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
		log.Fatal("could not get mounts:", err)
	}

	if err := unix.Mount("", "/", "", unix.MS_REMOUNT, ""); err != nil {
		log.Fatal("error remounting root as read/write:", err)
	}

	newRoot, err := prepareForPivot()
	if err != nil {
		log.Fatal("Error preparing for pivot root: ", err)
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
		log.Fatal("error while pivoting root:", err)
	}

	if err := unix.Chdir("/"); err != nil {
		log.Fatal(err)
	}

	if err := syscall.Exec("/sbin/init", os.Args, os.Environ()); err != nil {
		log.Fatal("error executing init:", err)
	}
}
