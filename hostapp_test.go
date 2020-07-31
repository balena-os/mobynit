package hostapp

import (
	"flag"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/containerfs"
)

var rootdir = flag.String("rootdir", "/path/to/rootdir", "Path to root directory")
var repeatedLabelsCount = flag.Int("repLabels", 0, "Number of containers with the same repeated label in root directory")

var mountedContainers int

func stubMount(layer.RWLayer, string) (containerfs.ContainerFS, error) {
	mountedContainers++
	if Debug {
		log.Printf("Stub rwLayer mount: %d\n", mountedContainers)
	}
	return nil, nil
}

func stubContainerMount(*Container, string) (string, error) {
	mountedContainers++
	if Debug {
		log.Printf("Stub container mount %d\n", mountedContainers)
	}
	return "", nil
}

func TestMountContainersByID(t *testing.T) {
	var tests = []struct {
		rootdir string
	}{
		{"/path/to/rootdir"},
	}

	savedContainerMount := containerMount
	containerMount = stubContainerMount
	defer func() { containerMount = savedContainerMount }()

	if *rootdir == "" {
		log.Fatal("This test requires a --rootdir flag")
	}

	tests[0].rootdir = *rootdir

	for _, test := range tests {
		current, err := os.Readlink(filepath.Join(test.rootdir, string(os.PathSeparator), "current"))
		if err != nil {
			log.Fatal("Could not get container ID:", err)
		}
		cid := filepath.Base(current)

		_, err = Mount(test.rootdir, cid)
		if err != nil {
			t.Errorf("Test with rootdir %s should have passed", test.rootdir)
		}
	}
}

func TestMountContainersByLabel(t *testing.T) {
	var tests = []struct {
		rootdir       string
		label         string
		expectFailure bool
	}{
		{"/does/not/exist", "None", true},
		{"/link/to/rootdir", "unique-label", false},
		{"/path/to/file", "None", true},
		{"/path/to/rootdir", "unique-label", false},
		{"/path/to/rootdir", "nonsense", false},
		{"/path/to/rootdir", "repeated-label", false},
	}

	savedRwLayerMount := rwLayerMount
	rwLayerMount = stubMount
	defer func() { rwLayerMount = savedRwLayerMount }()

	savedContainerMount := containerMount
	containerMount = stubContainerMount
	defer func() { containerMount = savedContainerMount }()

	if *rootdir == "" {
		log.Fatal("This test requires a --rootdir flag")
	}

	if *repeatedLabelsCount == 0 {
		log.Fatal("This test requires a --repLabels flag")
	}

	linkRootDir := "/tmp/testlink"
	if err := os.Symlink(*rootdir, linkRootDir); err != nil {
		log.Println("error creating rootdir symlink")
	}
	defer os.Remove(linkRootDir)

	fileRootDir, err := ioutil.TempFile("", "testHostAppFile")
	if err != nil {
		log.Fatal("Unable to create temporary file")
	}

	tests[1].rootdir = linkRootDir
	tests[2].rootdir = fileRootDir.Name()
	tests[3].rootdir = *rootdir
	tests[4].rootdir = *rootdir
	tests[5].rootdir = *rootdir

	for _, test := range tests {
		mountedContainers = 0
		containers, err := Mount(test.rootdir, test.label)
		if test.expectFailure == true && err == nil {
			t.Errorf("Test with rootdir %s and label %s should have failed", test.rootdir, test.label)
		} else if test.expectFailure == false && err != nil {
			t.Errorf("Test with rootdir %s and label %s should have passed", test.rootdir, test.label)
		}
		if test.label == "unique-label" && mountedContainers > 1 {
			t.Errorf("Test with rootdir %s and label %s should return just one container, not %d", test.rootdir, test.label, mountedContainers)
			if mountedContainers != len(containers) {
				t.Errorf("Test with rootdir %s and label %s should return just one container, not %d", test.rootdir, test.label, len(containers))
			}
		}
		if test.label == "repeated-label" && mountedContainers != *repeatedLabelsCount {
			t.Errorf("Test with rootdir %s and label %s should return %d containers, not %d", test.rootdir, test.label, repeatedLabelsCount, mountedContainers)
			if mountedContainers != len(containers) {
				t.Errorf("Test with rootdir %s and label %s should return %d containers, not %d", test.rootdir, test.label, mountedContainers, len(containers))
			}
		}
		if test.label == "nonsense" && mountedContainers != 0 {
			t.Errorf("Test with rootdir %s and label %s should return no containers mounted, not %d", test.rootdir, test.label, mountedContainers)
		}
		if test.label == "nonsense" && len(containers) != 0 {
			t.Errorf("Test with rootdir %s and label %s should return no containers mounted, not %d", test.rootdir, test.label, len(containers))
		}
	}
}

func BenchmarkMountSingleContainer(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := Mount("/path/to/rootdir", "unique-label"); err != nil {
			log.Println("Error: ", err)
		}
	}
}

func BenchmarkMountMultipleContainer(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := Mount("/path/to/rootdir", "repeated-label"); err != nil {
			log.Println("Error: ", err)
		}
	}
}

func ExampleMount() {
	if _, err := Mount("/path/to/rootdir", "unique-label"); err != nil {
		log.Println("Error: ", err)
	}
}
