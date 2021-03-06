/*
Copyright 2021 NetApp, Inc. All Rights Reserved.
Licensed under the Apache License, Version 2.0.
*/

package beegfs

import (
	"encoding/json"
	"net"
	"regexp"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

// These parameters have no effect when specified in the beeGFSClientConf configuration section.
var noEffectBeegfsConfOptions = []string{
	"sysMgmtdHost",
	"connClientPortUDP",
	"connPortShift",
}

// These parameters are unsupported when specified in the beeGFSClientConf configuration section.
var unsupportedBeegfsConfOptions = []string{
	"connInterfacesFile",
	"connNetFilterFile",
	"connTcpOnlyFilterFile",
	"connAuthFile",
}

// beegfsConfig contains all of the custom configuration (above and beyond whatever is in the beegfs-client.conf file)
// associated with a single BeeGFS file system EXCEPT for sysMgmtdHost, which is stored separately.
type beegfsConfig struct {
	ConnInterfaces    []string          `yaml:"connInterfaces"`
	ConnNetFilter     []string          `yaml:"connNetFilter"`
	ConnTcpOnlyFilter []string          `yaml:"connTcpOnlyFilter"`
	BeegfsClientConf  map[string]string `yaml:"beegfsClientConf"`
	connAuth          string            // unexported with no yaml tag so it cannot be set from a configuration file
}

func newBeegfsConfig() *beegfsConfig {
	return &beegfsConfig{
		BeegfsClientConf: make(map[string]string),
	}
}

// MarshalJSON overrides the default JSON encoding for the beegfsConfig struct. klogr uses JSON encoding to log
// struct values and thus implicitly calls this method. beegfsConfig does not export the connAuth field, so MarshalJSON
// encodes a new anonymous struct that includes an exported ConnAuth field and replaces it's value with "******" if
// it is not empty.
func (c beegfsConfig) MarshalJSON() ([]byte, error) {
	var connAuthString string
	if c.connAuth != "" {
		connAuthString = "******"
	}

	// See https://blog.gopheracademy.com/advent-2016/advanced-encoding-decoding/ for more context on how this works.
	type beegfsConfigAlias beegfsConfig // Use an alias to avoid an infinite loop and a stack overflow.
	return json.Marshal(&struct {
		// Use omitempty to avoid logging in "impossible" locations like DefaultConfig.
		// Use upper case ConnAuth because other similar fields are logged this way.
		ConnAuth          string `json:"ConnAuth,omitempty"`
		beegfsConfigAlias        // Embed the beegfsConfig type to avoid retyping all of its fields.
	}{
		ConnAuth:          connAuthString,
		beegfsConfigAlias: beegfsConfigAlias(c),
	})
}

// FileSystemSpecificConfig associates a beegfsConfig with a sysMgmtdHost.
type FileSystemSpecificConfig struct {
	SysMgmtdHost string       `yaml:"sysMgmtdHost"`
	Config       beegfsConfig `yaml:"config"`
}

// nodeSpecificConfig associates a default beegfsConfig and a list of file system specific configurations with a list
// of nodes.
type nodeSpecificConfig struct {
	NodeList                  []string                   `yaml:"nodeList"`
	DefaultConfig             beegfsConfig               `yaml:"config"`
	FileSystemSpecificConfigs []FileSystemSpecificConfig `yaml:"fileSystemSpecificConfigs"`
}

// PluginConfig contains a default beegfsConfig and a list of file system specific configurations. It is the
// configuration that is maintained for the life of the running plugin. It does NOT contain node specific
// configurations. The plugin creates its PluginConfig on startup by iterating through any  node specific
// configurations and accounting for those that apply to the node it is running on.
type PluginConfig struct {
	DefaultConfig             beegfsConfig               `yaml:"config"`
	FileSystemSpecificConfigs []FileSystemSpecificConfig `yaml:"fileSystemSpecificConfigs"`
}

// pluginConfigFromFile contains a PluginConfig and a list of node specific configurations. It is only used
// intermediately during the configuration file parsing process, as it may contain configurations that do NOT apply to
// the node the plugin is running on.
type pluginConfigFromFile struct {
	PluginConfig        `yaml:",inline"`     // embedded structs must be inlined
	NodeSpecificConfigs []nodeSpecificConfig `yaml:"nodeSpecificConfigs"`
}

// connAuthConfig associates a ConnAuth with a SysMgmtdHost.
type connAuthConfig struct {
	SysMgmtdHost string `yaml:"sysMgmtdHost"`
	ConnAuth     string `yaml:"connAuth"`
}

// MarshalJSON overrides the default JSON encoding for the connAuthConfig struct. klogr uses JSON encoding to log
// struct values and thus implicitly calls this method. MarshalJSON replaces connAuthConfig.ConnAuth with "******" if
// it is not empty.
func (c connAuthConfig) MarshalJSON() ([]byte, error) {
	var connAuthString string
	if c.ConnAuth != "" {
		connAuthString = "******"
	}

	// See https://blog.gopheracademy.com/advent-2016/advanced-encoding-decoding/ for more context on how this works.
	type connAuthConfigAlias connAuthConfig // Use an alias to avoid an infinite loop and a stack overflow.
	return json.Marshal(connAuthConfigAlias{SysMgmtdHost: c.SysMgmtdHost, ConnAuth: connAuthString})
}

// parseConfigFromFile reads the file at the specified path, unmarshalls it into a pluginConfigFromFile, and constructs
// a PluginConfig. It uses nodeID to determine if any node specific configuration applies to the node the plugin is
// running on. If it does, the final PluginConfig contains node specific overrides.
func parseConfigFromFile(path, nodeID string) (PluginConfig, error) {
	var rawConfig pluginConfigFromFile
	var newPluginConfig PluginConfig

	// read and parse configuration file
	// return immediately if an error occurs
	rawConfigBytes, err := fsutil.ReadFile(path)
	if err != nil {
		return PluginConfig{}, errors.Wrap(err, "failed to read configuration file")
	}
	if err := yaml.UnmarshalStrict(rawConfigBytes, &rawConfig); err != nil {
		return PluginConfig{}, errors.Wrap(err, "failed to unmarshal configuration file")
	}
	LogDebug(nil, "Raw configuration parsed", "parsePath", path, "rawConfig", rawConfig)

	// start populating newPluginConfig using values directly from rawConfig
	newPluginConfig = PluginConfig{
		DefaultConfig:             rawConfig.DefaultConfig,
		FileSystemSpecificConfigs: rawConfig.FileSystemSpecificConfigs,
	}

	// overwrite newPluginConfig with anything found in NodeSpecificConfigs pertaining to this node
	for _, nodeConfig := range rawConfig.NodeSpecificConfigs {
		appliesToNode := false
		for _, nodeName := range nodeConfig.NodeList {
			if nodeID == nodeName {
				appliesToNode = true
				break
			}
		}
		if appliesToNode {
			newPluginConfig.DefaultConfig.overwriteFrom(nodeConfig.DefaultConfig)
			newPluginConfig.FileSystemSpecificConfigs = overwriteFileSystemSpecificConfigs(
				newPluginConfig.FileSystemSpecificConfigs, nodeConfig.FileSystemSpecificConfigs)
		}
	}

	if err := newPluginConfig.validateConfig(); err != nil {
		return PluginConfig{}, errors.WithMessage(err, "config validation failed")
	}
	newPluginConfig.stripConfig()
	LogDebug(nil, "Actual configuration to be applied", "PluginConfig", newPluginConfig)

	return newPluginConfig, nil
}

// parseConnAuthFromFile reads the file at the specified path and modifies the provided PluginConfig so that it
// includes connAuth information.
func parseConnAuthFromFile(path string, newPluginConfig *PluginConfig) error {
	connAuthConfigs := make([]connAuthConfig, 0)
	rawConnAuthConfigBytes, err := fsutil.ReadFile(path)
	if err != nil {
		return errors.Wrap(err, "failed to read connAuth file")
	}
	if err := yaml.UnmarshalStrict(rawConnAuthConfigBytes, &connAuthConfigs); err != nil {
		return errors.Wrap(err, "failed to unmarshal connAuth file")
	}
	// The connAuthConfig.UnmarshallJSON method makes connAuthConfigs safe for logging.
	LogDebug(nil, "Raw connAuth configuration parsed", "parsePath", path,
		"connAuthConfigs", connAuthConfigs)

	for _, connAuth := range connAuthConfigs {
		foundMatchingConfig := false
		for i, specificConfig := range newPluginConfig.FileSystemSpecificConfigs {
			if connAuth.SysMgmtdHost == specificConfig.SysMgmtdHost {
				newPluginConfig.FileSystemSpecificConfigs[i].Config.connAuth = connAuth.ConnAuth
				foundMatchingConfig = true
				break
			}
		}
		if !foundMatchingConfig {
			newSpecificConfig := FileSystemSpecificConfig{
				SysMgmtdHost: connAuth.SysMgmtdHost,
				Config: beegfsConfig{
					connAuth: connAuth.ConnAuth,
				},
			}
			newPluginConfig.FileSystemSpecificConfigs = append(newPluginConfig.FileSystemSpecificConfigs, newSpecificConfig)
		}
	}

	// The pluginConfig.UnmashallJSON method makes newPluginConfig safe for logging.
	LogDebug(nil, "Actual configuration to be applied after parsing connAuth configuration",
		"PluginConfig", newPluginConfig)

	return nil
}

func (plConfig *PluginConfig) validateConfig() error {
	beegfsConfigs := []beegfsConfig{plConfig.DefaultConfig}
	// this regex is used to determine whether a given string is a domain name
	domainRegex := regexp.MustCompile("^(?:[_a-z0-9](?:[_a-z0-9-]{0,61}[a-z0-9]\\.)|(?:[0-9]+/[0-9]{2})\\.)+(?:[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?)?$")
	for _, config := range plConfig.FileSystemSpecificConfigs {
		// sysMgmtdHost can be localhost, an IP address, or a domain name. if it is none of these, return an error
		if config.SysMgmtdHost != "localhost" && net.ParseIP(config.SysMgmtdHost) == nil &&
			!domainRegex.MatchString(config.SysMgmtdHost) {
			return errors.Errorf("invalid SysMgmtdHost %s", config.SysMgmtdHost)
		}
		beegfsConfigs = append(beegfsConfigs, config.Config)
	}

	for _, config := range beegfsConfigs {
		for _, filter := range config.ConnNetFilter {
			if _, _, err := net.ParseCIDR(filter); err != nil && net.ParseIP(filter) == nil {
				return errors.Errorf("invalid ConnNetFilter %s", filter)
			}
		}
		for _, filter := range config.ConnTcpOnlyFilter {
			if _, _, err := net.ParseCIDR(filter); err != nil && net.ParseIP(filter) == nil {
				return errors.Errorf("invalid ConnTCPOnlyFilter %s", filter)
			}
		}
	}

	return nil
}

// stripConfig removes any no-effect beegfsConf options from the plugin configuration, logging a warning if any are
// found. It also logs a warning (but does not remove) any unsupported options it finds. See deployment.md for the list
// of no-effect options.
func (plConfig *PluginConfig) stripConfig() {
	beegfsConfigs := []beegfsConfig{plConfig.DefaultConfig}
	for _, config := range plConfig.FileSystemSpecificConfigs {
		beegfsConfigs = append(beegfsConfigs, config.Config)
	}
	for _, config := range beegfsConfigs {
		for _, noEffectOption := range noEffectBeegfsConfOptions {
			if val, present := config.BeegfsClientConf[noEffectOption]; present {
				LogDebug(nil, "WARNING: No-effect beegfs configuration option found and removed from config",
					"noEffectOption", noEffectOption, "noEffectValue", val)
				delete(config.BeegfsClientConf, noEffectOption)
			}
		}
		for _, unsupportedOption := range unsupportedBeegfsConfOptions {
			if val, present := config.BeegfsClientConf[unsupportedOption]; present {
				LogDebug(nil, "WARNING: Unsupported beegfs configuration option found and left in config",
					"unsupportedOption", unsupportedOption, "unsupportedValue", val)
			}
		}
	}
}

// overwriteFileSystemSpecificConfigs looks for FileSystemSpecificConfigs in writeTo and writeFrom with the same
// sysMgmtdHost. When it finds a match, overwriteFileSystemSpecificConfigs ONLY overwrites configuration in writeTo
// that is also defined in writeFrom.
func overwriteFileSystemSpecificConfigs(writeTo, writeFrom []FileSystemSpecificConfig) []FileSystemSpecificConfig {
	for _, writeFromConfig := range writeFrom {
		writeToHadConfig := false
		for i, writeToConfig := range writeTo { // use index to modify writeTo in place
			if writeToConfig.SysMgmtdHost == writeFromConfig.SysMgmtdHost {
				writeToHadConfig = true
				writeTo[i].Config.overwriteFrom(writeFromConfig.Config)
			}
		}
		if writeToHadConfig == false {
			writeTo = append(writeTo, writeFromConfig)
		}
	}
	return writeTo
}

// overwriteFrom ONLY overwrites configuration in the receiving beegfsConfig that is also defined in writeFrom, while
// leaving writeFrom configuration untouched.
func (c *beegfsConfig) overwriteFrom(writeFrom beegfsConfig) {
	if len(writeFrom.ConnInterfaces) != 0 {
		c.ConnInterfaces = make([]string, len(writeFrom.ConnInterfaces))
		copy(c.ConnInterfaces, writeFrom.ConnInterfaces)
	}
	if len(writeFrom.ConnNetFilter) != 0 {
		c.ConnNetFilter = make([]string, len(writeFrom.ConnNetFilter))
		copy(c.ConnNetFilter, writeFrom.ConnNetFilter)
	}
	if len(writeFrom.ConnTcpOnlyFilter) != 0 {
		c.ConnTcpOnlyFilter = make([]string, len(writeFrom.ConnTcpOnlyFilter))
		copy(c.ConnTcpOnlyFilter, writeFrom.ConnTcpOnlyFilter)
	}
	if writeFrom.connAuth != "" {
		c.connAuth = writeFrom.connAuth
	}
	for k, v := range writeFrom.BeegfsClientConf {
		c.BeegfsClientConf[k] = v
	}
}
