package daemon

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/docker/docker/archive"
	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/docker/pkg/symlink"
)

type Volume struct {
	HostPath    string
	VolPath     string
	isReadWrite bool
	isBindMount bool
}

func (v *Volume) isDir() (bool, error) {
	stat, err := os.Stat(v.HostPath)
	if err != nil {
		return false, err
	}

	return stat.IsDir(), nil
}

func prepareVolumesForContainer(container *Container) error {
	if container.Volumes == nil || len(container.Volumes) == 0 {
		container.Volumes = make(map[string]string)
		container.VolumesRW = make(map[string]bool)
		if err := applyVolumesFrom(container); err != nil {
			return err
		}
	}

	return createVolumes(container)
}

func setupMountsForContainer(container *Container) error {
	mounts := []execdriver.Mount{
		{
			Source:      container.ResolvConfPath,
			Destination: "/etc/resolv.conf",
			Writable:    true,
			Slave:       true,
		},
	}

	if container.HostnamePath != "" {
		mounts = append(mounts, execdriver.Mount{
			Source:      container.HostnamePath,
			Destination: "/etc/hostname",
			Writable:    true,
			Private:     true,
		})
	}

	if container.HostsPath != "" {
		mounts = append(mounts, execdriver.Mount{
			Source:      container.HostsPath,
			Destination: "/etc/hosts",
			Writable:    true,
			Slave:       true,
		})
	}

	// Mount user specified volumes
	// Note, these are not private because you may want propagation of (un)mounts from host
	// volumes. For instance if you use -v /usr:/usr and the host later mounts /usr/share you
	// want this new mount in the container
	// These mounts must be ordered based on the length of the path that it is being mounted to (lexicographic)
	for _, path := range container.sortedVolumeMounts() {
		mounts = append(mounts, execdriver.Mount{
			Source:      container.Volumes[path],
			Destination: path,
			Writable:    container.VolumesRW[path],
		})
	}

	container.command.Mounts = mounts
	return nil
}

// sortedVolumeMounts returns the list of container volume mount points sorted in lexicographic order
func (container *Container) sortedVolumeMounts() []string {
	var mountPaths []string
	for path := range container.Volumes {
		mountPaths = append(mountPaths, path)
	}

	sort.Strings(mountPaths)
	return mountPaths
}

func parseVolumesFromSpec(container *Container, spec string) (map[string]*Volume, error) {
	specParts := strings.SplitN(spec, ":", 2)
	if len(specParts) == 0 {
		return nil, fmt.Errorf("Malformed volumes-from specification: %s", spec)
	}

	c := container.daemon.Get(specParts[0])
	if c == nil {
		return nil, fmt.Errorf("Container %s not found. Impossible to mount its volumes", specParts[0])
	}

	volumes, err := c.GetVolumes()
	if err != nil {
		return nil, err
	}

	if len(specParts) == 2 {
		mode := specParts[1]
		if validVolumeMode(mode) {
			return nil, fmt.Errorf("Invalid mode for volumes-from: %s", mode)
		}

		// Set the mode for the inheritted volume
		for _, v := range volumes {
			v.isReadWrite = mode != "ro"
		}
	}

	return volumes, nil
}

func applyVolumesFrom(container *Container) error {
	volumesFrom := container.hostConfig.VolumesFrom

	for _, spec := range volumesFrom {
		volumes, err := parseVolumesFromSpec(container, spec)
		if err != nil {
			return err
		}

		for _, v := range volumes {
			if err = v.initialize(container); err != nil {
				return err
			}
		}
	}
	return nil
}

func validVolumeMode(mode string) bool {
	validModes := map[string]bool{
		"rw": true,
		"ro": true,
	}

	return validModes[mode]
}

func parseBindVolumeSpec(spec string) (Volume, error) {
	var (
		arr = strings.Split(spec, ":")
		vol Volume
	)

	switch len(arr) {
	case 1:
		vol.VolPath = spec
		vol.isReadWrite = true
	case 2:
		vol.HostPath = arr[0]
		vol.VolPath = arr[1]
		vol.isReadWrite = true
	case 3:
		vol.HostPath = arr[0]
		vol.VolPath = arr[1]
		vol.isReadWrite = validVolumeMode(arr[2]) && arr[2] == "rw"
	default:
		return vol, fmt.Errorf("Invalid volume specification: %s", spec)
	}

	if !filepath.IsAbs(vol.HostPath) {
		return vol, fmt.Errorf("cannot bind mount volume: %s volume paths must be absolute.", vol.HostPath)
	}

	return vol, nil
}

func createVolumes(container *Container) error {
	// Get all the bindmounts
	volumes, err := container.GetVolumes()
	if err != nil {
		return err
	}

	// Get all the rest of the volumes
	for volPath := range container.Config.Volumes {
		// Make sure the the volume isn't already specified as a bindmount
		if _, exists := volumes[volPath]; !exists {
			volumes[volPath] = &Volume{
				VolPath:     volPath,
				isReadWrite: true,
				isBindMount: false,
			}
		}
	}

	for _, vol := range volumes {
		if err = vol.initialize(container); err != nil {
			return err
		}
	}

	return nil
}

func createVolumeHostPath(container *Container) (string, error) {
	volumesDriver := container.daemon.volumes.Driver()

	// Do not pass a container as the parameter for the volume creation.
	// The graph driver using the container's information ( Image ) to
	// create the parent.
	c, err := container.daemon.volumes.Create(nil, "", "", "", "", nil, nil)
	if err != nil {
		return "", err
	}
	hostPath, err := volumesDriver.Get(c.ID, "")
	if err != nil {
		return hostPath, fmt.Errorf("Driver %s failed to get volume rootfs %s: %s", volumesDriver, c.ID, err)
	}

	return hostPath, nil
}

func (v *Volume) initialize(container *Container) error {
	var err error
	v.VolPath = filepath.Clean(v.VolPath)

	// Do not initialize an existing volume
	if _, exists := container.Volumes[v.VolPath]; exists {
		return nil
	}

	// If it's not a bindmount we need to create the dir on the host
	if !v.isBindMount && v.HostPath == "" {
		v.HostPath, err = createVolumeHostPath(container)
		if err != nil {
			return err
		}
	}

	hostPath, err := filepath.EvalSymlinks(v.HostPath)
	if err != nil {
		return err
	}

	// Create the mountpoint
	// This is the path to the volume within the container FS
	// This differs from `hostPath` in that `hostPath` refers to the place where
	// the volume data is actually stored on the host
	fullVolPath, err := symlink.FollowSymlinkInScope(filepath.Join(container.basefs, v.VolPath), container.basefs)
	if err != nil {
		return err
	}

	container.Volumes[v.VolPath] = hostPath
	container.VolumesRW[v.VolPath] = v.isReadWrite

	volIsDir, err := v.isDir()
	if err != nil {
		return err
	}
	if err := createIfNotExists(fullVolPath, volIsDir); err != nil {
		return err
	}

	// Do not copy or change permissions if we are mounting from the host
	if v.isReadWrite && !v.isBindMount {
		return copyExistingContents(fullVolPath, hostPath)
	}
	return nil
}

func createIfNotExists(destination string, isDir bool) error {
	if _, err := os.Stat(destination); err == nil || !os.IsNotExist(err) {
		return nil
	}

	if isDir {
		return os.MkdirAll(destination, 0755)
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}

	f, err := os.OpenFile(destination, os.O_CREATE, 0755)
	if err != nil {
		return err
	}
	f.Close()

	return nil
}

func copyExistingContents(source, destination string) error {
	volList, err := ioutil.ReadDir(source)
	if err != nil {
		return err
	}

	if len(volList) > 0 {
		srcList, err := ioutil.ReadDir(destination)
		if err != nil {
			return err
		}

		if len(srcList) == 0 {
			// If the source volume is empty copy files from the root into the volume
			if err := archive.CopyWithTar(source, destination); err != nil {
				return err
			}
		}
	}

	return copyOwnership(source, destination)
}

// copyOwnership copies the permissions and uid:gid of the source file
// into the destination file
func copyOwnership(source, destination string) error {
	var stat syscall.Stat_t

	if err := syscall.Stat(source, &stat); err != nil {
		return err
	}

	if err := os.Chown(destination, int(stat.Uid), int(stat.Gid)); err != nil {
		return err
	}

	return os.Chmod(destination, os.FileMode(stat.Mode))
}
