package hostapp

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/idtools"
	"golang.org/x/sys/unix"

	_ "github.com/docker/docker/daemon/graphdriver/overlay2"
)

type HostConfig struct {
	Labels map[string]string `json:"Labels"`
}

type Config struct {
	HostConfig `json:"Config"`

	ID     string `json:"ID"`
	Image  string `json:"Image"`
	Name   string `json:"Name"`
	Driver string `json:"Driver"`
}

type Container struct {
	Config
	MountPath string
}

var (
	// Debug enables more verbose logging
	Debug bool = false
	// Verbose enables verbose logging
	Verbose bool = false
)

// Testing stub substitution
var (
	rwLayerMount   = layer.RWLayer.Mount // As root, do not mount layer
	containerMount = (*Container).mount  // As user, do not call mount
)

func (container *Container) mount(layer_root string) (string, error) {
	ls, err := layer.NewStoreFromOptions(layer.StoreOptions{
		Root:                      layer_root,
		MetadataStorePathTemplate: filepath.Join(layer_root, "image", "%s", "layerdb"),
		IDMapping:                 &idtools.IdentityMapping{},
		GraphDriver:               container.Config.Driver,
		OS:                        runtime.GOOS,
	})
	if err != nil {
		return "", fmt.Errorf("error loading layer store: %v", err)
	}

	rwlayer, err := ls.GetRWLayer(container.Config.ID)
	if err != nil {
		return "", fmt.Errorf("error getting container layer: %v", err)
	}

	newRoot, err := rwLayerMount(rwlayer, "")
	if err != nil {
		return "", fmt.Errorf("error mounting container fs: %v", err)
	}
	container.MountPath = newRoot.Path()

	if err := unix.Mount("", container.MountPath, "", unix.MS_REMOUNT, ""); err != nil {
		return "", fmt.Errorf("error remounting container as read/write: %v", err)
	}
	if Debug {
		log.Printf("Mounted ID %s in %s\n", container.Config.ID, container.MountPath)
	}

	return container.MountPath, nil
}

func (container *Container) initialize(homePath string) error {
	configPath := filepath.Join(homePath, string(os.PathSeparator), "config.v2.json")
	f, err := os.Open(configPath)
	if err != nil {
		fmt.Printf("%s\n", err)
		return err
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(&container.Config); err != nil {
		log.Println("Error decoding config file:", err)
		return err
	}
	if Verbose || Debug {
		log.Println("Initialized container:", container.Config.Name)
	}
	if Debug {
		log.Printf("%#+v\n", container.Config)
	}
	return nil
}

func (container *Container) mountOverlayByID(mountRoot string, targetID string) (string, error) {
	if container.ID == targetID {
		if Verbose {
			log.Printf("Mounted %s in %s\n", targetID, mountRoot)
		}
		newRootPath, err := containerMount(container, mountRoot)
		return newRootPath, err
	}
	return "", fmt.Errorf("ID %s not found\n", targetID)
}

func (container *Container) mountOverlayByLabel(mountRoot string, targetLabel string) (string, error) {
	if Debug {
		log.Println("Searching for label", targetLabel)
	}
	for label, value := range container.Labels {
		if label == targetLabel {
			if value == "overlay" {
				if Verbose {
					log.Printf("Mounted %s in %s\n", container.Config.Name, mountRoot)
				}
				newRootPath, err := containerMount(container, mountRoot)
				return newRootPath, err
			}
		}
	}
	return "", fmt.Errorf("Label %s not found", targetLabel)
}

func initializeContainers(rootdir string, match string) ([]Container, error) {
	containersDir := filepath.Join(rootdir, "containers")
	dirs, err := ioutil.ReadDir(containersDir)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	Containers := make([]Container, 0)
	mountedContainers := make([]Container, 0)
	for i, dir := range dirs {
		if Debug {
			log.Println("Looking in", dir.Name())
		}
		if dir.IsDir() {
			homePath := filepath.Join(containersDir, string(os.PathSeparator), dir.Name())
			Containers = append(Containers, Container{})
			if Containers[i].initialize(homePath) != nil {
				log.Println("Error initializing container")
				return nil, err
			}
			if Debug {
				log.Printf("Trying to mount %s from %s\n", match, homePath)
			}
			if Containers[i].ID == match {
				_, err := Containers[i].mountOverlayByID(rootdir, match)
				if err != nil {
					log.Println("Failed to mount container:", err)
				} else {
					mountedContainers = append(mountedContainers, Containers[i])
				}
			} else {
				_, err := Containers[i].mountOverlayByLabel(rootdir, match)
				if err == nil {
					mountedContainers = append(mountedContainers, Containers[i])
				}
			}
		}
	}
	return mountedContainers, err
}

// Mount a container union filesystem matching either by ID or label
func Mount(rootdir string, label string) ([]Container, error) {
	if Debug {
		log.Printf("Searching for container with ID/label %s in root directory %s\n", label, rootdir)
	}
	return initializeContainers(rootdir, label)
}
