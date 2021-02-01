package beegfs

import (
	"crypto/sha1"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/spf13/afero"
	"go.uber.org/multierr"
	"gopkg.in/ini.v1"
	"k8s.io/utils/mount"
)

// We use afero to abstract the file system. During unit tests, we use an in-memory file system (afero.NewMemFs). In an
// actual deployment, we use the host's file system.
var fs = afero.NewOsFs()
var fsutil = afero.Afero{Fs: fs}

// newBeegfsUrl converts the sysMgmtdHost and path into a URL with the format beegfs://host/path.
func newBeegfsUrl(host string, path string) string {
	structURL := url.URL{
		Scheme: "beegfs",
		Host:   host,
		Path:   path,
	}
	return structURL.String()
}

// parseBeegfsUrl parses a URL with the format beegfs://host/path and returns the sysMgmtdHost and path.
func parseBeegfsUrl(rawUrl string) (sysMgmtdHost string, path string, err error) {
	var structUrl *url.URL
	if structUrl, err = url.Parse(rawUrl); err != nil {
		return "", "", err
	}
	if structUrl.Scheme != "beegfs" {
		return "", "", errors.New("URL has incorrect scheme")
	}
	// TODO(webere) more checks for bad values
	return structUrl.Host, structUrl.Path, nil
}

// writeClientFiles writes a beegfs-client.conf file and optionally a connInterfacesFile, a connNetFilterFile, and a
// connTcpOnlyFilterFile to a beegfsVolume's mountDirPath. The beegfs-client.conf file is generated by reading in
// an existing beegfs-client.conf file at confTemplatePath and overriding its values with those specified in the
// beegfsVolume's config. writeClientFiles assumes an empty directory has already been created at mountDirPath.
func writeClientFiles(vol beegfsVolume, confTemplatePath string) (err error) {
	glog.V(LogDebug).Infof("Writing client files from %s to %s", confTemplatePath, vol.volumeID)
	connInterfacesFilePath := path.Join(vol.mountDirPath, "connInterfacesFile")
	connNetFilterFilePath := path.Join(vol.mountDirPath, "connNetFilterFile")
	connTcpOnlyFilterFilePath := path.Join(vol.mountDirPath, "connTcpOnlyFilterFile")

	// setConfigValueIfKeyExists is a helper function used to get around the fact that the go-ini library will allows
	// setting the value of an arbitrary key, even if the key did not exist in the original .ini file.
	// setConfigValueIfKeyExists returns an error if the supplied key did not exist in the original .ini file.
	setConfigValueIfKeyExists := func(iniFile *ini.File, key, value string) error {
		if iniFile.Section("").HasKey(key) {
			iniFile.Section("").Key(key).SetValue(value)
		} else {
			return errors.Errorf("%v not in template beegfs-client.conf file", key)
		}
		return nil
	}

	// TODO(eastburj): patch BeeGFS to support setting connClientPortUDP to zero
	// TODO(webere): document that connClientPortUDP is NOT a supported option (maybe a range?)
	// select a UDP port to use for this volume mount
	var connClientPortUDP string
	port, err := getEphemeralPortUDP()
	if err != nil {
		return errors.WithMessage(err, "error selecting connClientPortUDP")
	}
	connClientPortUDP = strconv.Itoa(port)

	// TODO (webere): consider loading the template globally only once
	var clientConfBytes []byte
	var clientConfINI *ini.File
	if clientConfBytes, err = fsutil.ReadFile(confTemplatePath); err != nil {
		return errors.Wrapf(err, "error loading beegfs-client.conf file at %s", confTemplatePath)
	}
	if clientConfINI, err = ini.Load(clientConfBytes); err != nil {
		return errors.Wrap(err, "error parsing template beegfs-client.conf file")
	}
	if err = setConfigValueIfKeyExists(clientConfINI, "sysMgmtdHost", vol.sysMgmtdHost); err != nil {
		return err
	}
	if err = setConfigValueIfKeyExists(clientConfINI, "connClientPortUDP", connClientPortUDP); err != nil {
		return err
	}
	for k, v := range vol.config.BeegfsClientConf {
		if err := setConfigValueIfKeyExists(clientConfINI, k, v); err != nil {
			return err
		}
	}

	if len(vol.config.ConnInterfaces) != 0 {
		connInterfacesFileContents := strings.Join(vol.config.ConnInterfaces, "\n") + "\n"
		if err := setConfigValueIfKeyExists(clientConfINI, "connInterfacesFile", connInterfacesFilePath); err != nil {
			return err
		}
		if err = fsutil.WriteFile(connInterfacesFilePath, []byte(connInterfacesFileContents), 0644); err != nil {
			return errors.Wrap(err, "error writing connInterfaces file")
		}
	}

	if len(vol.config.ConnNetFilter) != 0 {
		connNetFilterFileContents := strings.Join(vol.config.ConnNetFilter, "\n") + "\n"
		if err := setConfigValueIfKeyExists(clientConfINI, "connNetFilterFile", connNetFilterFilePath); err != nil {
			return err
		}
		if err = fsutil.WriteFile(connNetFilterFilePath, []byte(connNetFilterFileContents), 0644); err != nil {
			return errors.Wrap(err, "error writing connNetFilter file")
		}
	}

	if len(vol.config.ConnTcpOnlyFilter) != 0 {
		connTcpOnlyFilterFileContents := strings.Join(vol.config.ConnTcpOnlyFilter, "\n") + "\n"
		if err := setConfigValueIfKeyExists(clientConfINI, "connTcpOnlyFilterFile", connTcpOnlyFilterFilePath); err != nil {
			return err
		}
		if err = fsutil.WriteFile(connTcpOnlyFilterFilePath, []byte(connTcpOnlyFilterFileContents), 0644); err != nil {
			return errors.Wrap(err, "error writing connTcpOnlyFilter file")
		}
	}

	var clientConfFileHandle afero.File
	if clientConfFileHandle, err = fs.Create(vol.clientConfPath); err != nil {
		return errors.Wrap(err, "error creating beegfs-client.conf file")
	}
	if _, err = clientConfINI.WriteTo(clientConfFileHandle); err != nil {
		return errors.Wrap(err, "error writing beegfs-client.conf file")
	}

	return nil
}

// squashConfigForSysMgmtdHost takes a sysMgmtdHost and pluginConfig, which MAY have FileSystemSpecificConfigs. If
// the pluginConfig contains overrides for the provided sysMgmtdHost, squashConfigForSysMgmtdHost combines them with
// the DefaultConfig (giving preference to the appropriate fileSystemSpecificConfig). Otherwise, it returns the
// DefaultConfig.
func squashConfigForSysMgmtdHost(sysMgmtdHost string, config pluginConfig) (returnConfig beegfsConfig) {
	returnConfig = *newBeegfsConfig()
	returnConfig.overwriteFrom(config.DefaultConfig)
	for _, fileSystemSpecificConfig := range config.FileSystemSpecificConfigs {
		if sysMgmtdHost == fileSystemSpecificConfig.SysMgmtdHost {
			returnConfig.overwriteFrom(fileSystemSpecificConfig.Config)
		}
	}
	return returnConfig
}

// mountIfNecessary mounts a BeeGFS file system to vol.mountPath assuming configuration files have been written to
// vol.mountDirPath by writeClientFiles.
func mountIfNecessary(vol beegfsVolume, mounter mount.Interface) (err error) {
	glog.V(LogDebug).Infof("Mounting volume %s if necessary", vol.volumeID)
	// TODO (webere): Support mount options
	mountOpts := []string{"rw", "relatime", "cfgFile=" + vol.clientConfPath}

	// Check to make sure file system is not already mounted.
	notMnt, err := mounter.IsLikelyNotMountPoint(vol.mountPath)
	if err != nil {
		if os.IsNotExist(err) {
			// the file system can't be mounted because the mount point hasn't been created
			if err = os.Mkdir(vol.mountPath, 0750); err != nil {
				return errors.WithStack(err)
			}
			notMnt = true
		} else {
			return errors.WithStack(err)
		}
	}

	if !notMnt {
		// The filesystem is already mounted. There is nothing to do.
		return errors.WithStack(err)
	}

	glog.V(LogDebug).Infof("Mounting BeeGFS to %s", vol.mountPath)
	if err = mounter.Mount("beegfs_nodev", vol.mountPath, "beegfs", mountOpts); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

// unmountAndCleanUpIfNecessary cleans up a mounted BeeGFS filesystem ONLY if it is not bind mounted somewhere
// else. This is necessary to avoid trying to unmount a BeeGFS filesystem that is still in use by some container.
// "Cleans up" in this context means unmounts the BeeGFS filesystem, deletes the mount point (mountPath), and deletes
// all files under mountDirPath. unmountAndCleanUpIfNecessary also deletes mountDirPath if rmDir is set to true.
// unmountAndCleanUpIfNecessary quietly continues WITHOUT error if the BeeGFS filesystem is not mounted.
func unmountAndCleanUpIfNecessary(vol beegfsVolume, rmDir bool, mounter mount.Interface) (err error) {
	glog.V(LogDebug).Infof("Unmounting volume %s and cleaning up if necessary", vol.volumeID)
	// Decide whether or not to unmount BeeGFS filesystem by checking whether it is bind mounted somewhere else. We
	// cannot use beegfsMounter.GetRefs() because we are bind mounting subdirectories (e.g. .../volume1/mount is the
	// initial mount point but .../volume1/mount/volume1 is the directory we bind mount). beegfsMounter.GetRefs() is
	// incapable of discovering this.
	allMounts, err := mounter.List()
	if err != nil {
		return errors.Wrap(err, "error listing mounted filesystems")
	}
	for _, entry := range allMounts {
		// Our container mounts the host's root filesystem at /host (like /:/host), so a file system might appear to be
		// mounted at both /path/to/file/system and /host/path/to/file/system. These duplicates are NOT bind mounts, so
		// we use !strings.Contains() instead of entry.Path != mountPath below.
		if entry.Device == "beegfs_nodev" && !strings.Contains(entry.Path, vol.mountPath) {
			for _, opt := range entry.Opts {
				if strings.Contains(opt, vol.clientConfPath) {
					// This is a bind mount of the BeeGFS filesystem mounted at mountPath
					return errors.Errorf("refused to unmount staged file system at %v while bind mounted at %v",
						vol.mountPath, entry.Path)
				}
			}
		}
	}

	if err = mount.CleanupMountPoint(vol.mountPath, mounter, false); err != nil {
		return errors.WithStack(err)
	}
	if err = cleanUpIfNecessary(vol, rmDir); err != nil {
		return errors.WithMessagef(err, "error cleaning up volume %v", vol.volumeID)
	}
	return nil
}

// cleanUpIfNecessary deletes all files associated with a beegfsVolume (in vol.mountDirPath) that is not mounted. It
// also deletes vol.mountDirPath if rmDir is set to true.
func cleanUpIfNecessary(vol beegfsVolume, rmDir bool) (err error) {
	glog.V(LogDebug).Infof("Cleaning up volume %s if necessary", vol.volumeID)
	if rmDir == false {
		dir, err := ioutil.ReadDir(vol.mountDirPath)
		if err != nil {
			return errors.WithStack(err)
		}
		for _, d := range dir {
			if err = fs.RemoveAll(path.Join(vol.mountDirPath, d.Name())); err != nil {
				return errors.WithStack(err)
			}
		}
	} else {
		if err = fs.RemoveAll(vol.mountDirPath); err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

// getEphemeralPortUDP either returns an error or the system-assigned ephemeral port of a temporary UDP/IPv4 socket bound to INADDR_ANY.
// Note: This only exists because BeeGFS does not support setting connClientPortUDP to zero.
// Warning: Other processes on the host may bind the port returned before BeeGFS binds it.  Calling this method in a retry loop may mitigate that issue.  Ideally, BeeGFS itself should be patched to support binding to port zero.
func getEphemeralPortUDP() (port int, err error) {
	conn, err := net.ListenPacket("udp4", "")
	if err != nil {
		err = errors.WithStack(err)
		return 0, err
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			closeErr = errors.WithStack(closeErr)
			if err != nil {
				err = multierr.Append(err, closeErr)
			} else {
				err = closeErr
			}
		}
	}()
	lAddr := conn.LocalAddr()
	lUDPAddr, err := net.ResolveUDPAddr(lAddr.Network(), lAddr.String())
	if err != nil {
		err = errors.WithStack(err)
		return 0, err
	}
	return lUDPAddr.Port, nil
}

// sanitizeVolumeID takes a volumeID like beegfs://127.0.0.1/scratch/vol1 and returns a string like
// 127.0.0.1_scratch_vol1. It is primarily used to generate sane directory names for the controller service, but may
// find other uses. sanitizeVolumeID replaces any _ in the provided volumeID with __ in the output to reduce ambiguity.
// sanitizeVolumeID returns a sha1 hash of the volumeID if the sanitized volumeID would be over 255 characters (the
// length limit for a file name in many file systems).
func sanitizeVolumeID(volumeID string) string {
	sanitizedVolumeID := strings.Replace(volumeID, "beegfs://", "", 1)
	sanitizedVolumeID = strings.Replace(sanitizedVolumeID, "_", "__", -1) // preserve existing _ as __
	sanitizedVolumeID = strings.Replace(sanitizedVolumeID, "/", "_", -1)
	if len(sanitizedVolumeID) > 255 {
		return fmt.Sprintf("%x", sha1.Sum([]byte(volumeID)))
	}
	return sanitizedVolumeID
}
