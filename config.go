package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jjcinaz/serverconfig"
	"gopkg.in/yaml.v3"
)

const defaultConfigPath = "/etc/clustercheck/config.yaml"

type clustercheckConfig struct {
	Server   httpServerSettings         `yaml:"http"`
	Database serverconfig.MySQLDatabase `yaml:"database"`
	Cluster  clusterSettings            `yaml:"cluster"`
	Valkey   valkeyHealthConfig         `yaml:"valkey"`
	Proxmox  proxmoxConfig              `yaml:"proxmox"`
}

type httpServerSettings struct {
	BindAddress    string `yaml:"bind_address" env:"BIND_ADDRESS"`
	bindAddressSet bool
}

type clusterSettings struct {
	ExpectedSize int `yaml:"expected_size" env:"EXPECTED_CLUSTER_SIZE"`
}

type proxmoxConfig struct {
	Endpoint    string        `yaml:"endpoint" env:"PROXMOX_ENDPOINT"`
	APIToken    string        `yaml:"api_token" env:"PROXMOX_API_TOKEN"`
	Timeout     time.Duration `yaml:"timeout" env:"PROXMOX_TIMEOUT"`
	Enabled     bool          `yaml:"-"`
	endpointSet bool
	apiTokenSet bool
	timeoutSet  bool
}

func configPath() string {
	var path string

	path = os.Getenv("CONFIG")
	if path == "" {
		path = defaultConfigPath
	}
	return path
}

func yamlKeySet(value *yaml.Node, key string) bool {
	return yamlNodeForKey(value, key) != nil
}

func yamlNodeForKey(value *yaml.Node, key string) *yaml.Node {
	var (
		i int
	)

	if value.Kind != yaml.MappingNode {
		return nil
	}
	for i = 0; i < len(value.Content)-1; i += 2 {
		if value.Content[i].Value == key {
			return value.Content[i+1]
		}
	}
	return nil
}

func (config *clustercheckConfig) UnmarshalYAML(value *yaml.Node) error {
	type clustercheckConfigRaw clustercheckConfig
	var (
		decoded    clustercheckConfigRaw
		serverNode *yaml.Node
		err        error
	)

	if err = value.Decode(&decoded); err != nil {
		return err
	}
	*config = clustercheckConfig(decoded)
	serverNode = yamlNodeForKey(value, "server")
	if serverNode != nil {
		if err = serverNode.Decode(&config.Server); err != nil {
			return err
		}
	}
	return nil
}

func (config *httpServerSettings) UnmarshalYAML(value *yaml.Node) error {
	type httpServerSettingsRaw httpServerSettings
	var decoded httpServerSettingsRaw
	var err error

	if err = value.Decode(&decoded); err != nil {
		return err
	}
	*config = httpServerSettings(decoded)
	config.bindAddressSet = yamlKeySet(value, "bind_address")
	return nil
}

func (config *valkeyHealthConfig) UnmarshalYAML(value *yaml.Node) error {
	type valkeyHealthConfigRaw valkeyHealthConfig
	var decoded valkeyHealthConfigRaw
	var err error

	if err = value.Decode(&decoded); err != nil {
		return err
	}
	*config = valkeyHealthConfig(decoded)
	config.sentinelAddrsSet = yamlKeySet(value, "sentinel_addrs")
	config.masterNameSet = yamlKeySet(value, "master_name")
	config.expectedNodesSet = yamlKeySet(value, "expected_nodes")
	config.timeoutSet = yamlKeySet(value, "timeout")
	return nil
}

func (config *proxmoxConfig) UnmarshalYAML(value *yaml.Node) error {
	type proxmoxConfigRaw proxmoxConfig
	var decoded proxmoxConfigRaw
	var err error

	if err = value.Decode(&decoded); err != nil {
		return err
	}
	*config = proxmoxConfig(decoded)
	config.endpointSet = yamlKeySet(value, "endpoint")
	config.apiTokenSet = yamlKeySet(value, "api_token")
	config.timeoutSet = yamlKeySet(value, "timeout")
	return nil
}

func validateHTTPServerSettings(config *httpServerSettings) error {
	var (
		portRaw string
		port    int
		err     error
	)

	if !config.bindAddressSet {
		if config.BindAddress == "" {
			config.BindAddress = ":8080"
		}
	}
	config.BindAddress = strings.TrimSpace(config.BindAddress)
	if config.BindAddress == "" {
		return fmt.Errorf("server.bind_address must not be blank")
	}
	_, portRaw, err = net.SplitHostPort(config.BindAddress)
	if err != nil {
		return fmt.Errorf("server.bind_address must be formatted as host:port: %w", err)
	}
	if portRaw == "" {
		return fmt.Errorf("server.bind_address must include a port")
	}
	port, err = strconv.Atoi(portRaw)
	if err != nil {
		return fmt.Errorf("server.bind_address port must be numeric")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("server.bind_address port must be between 1 and 65535")
	}
	return nil
}

func validateClusterSettings(config *clusterSettings) error {
	if config.ExpectedSize < 1 {
		return fmt.Errorf("cluster.expected_size must be a positive integer")
	}
	return nil
}

func validateProxmoxConfig(config *proxmoxConfig) error {
	var (
		endpoint *url.URL
		err      error
	)

	config.Endpoint = strings.TrimRight(strings.TrimSpace(config.Endpoint), "/")
	config.APIToken = strings.TrimSpace(config.APIToken)
	config.Enabled = config.Endpoint != "" || config.APIToken != "" || config.endpointSet || config.apiTokenSet
	if !config.timeoutSet && config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}
	if !config.Enabled {
		return nil
	}
	if config.Endpoint == "" {
		return fmt.Errorf("proxmox.endpoint must be set when proxmox.api_token is set")
	}
	if config.APIToken == "" {
		return fmt.Errorf("proxmox.api_token must be set when proxmox.endpoint is set")
	}
	endpoint, err = url.Parse(config.Endpoint)
	if err != nil {
		return fmt.Errorf("proxmox.endpoint must be a valid URL: %w", err)
	}
	if endpoint.Scheme != "https" && endpoint.Scheme != "http" {
		return fmt.Errorf("proxmox.endpoint must use http or https")
	}
	if endpoint.Host == "" {
		return fmt.Errorf("proxmox.endpoint must include a host")
	}
	if config.Timeout <= 0 {
		return fmt.Errorf("proxmox.timeout must be positive")
	}
	return nil
}

func validateConfig(config *clustercheckConfig) error {
	var err error

	if err = validateHTTPServerSettings(&config.Server); err != nil {
		return err
	}
	if err = validateClusterSettings(&config.Cluster); err != nil {
		return err
	}
	if err = validateValkeyHealthConfig(&config.Valkey); err != nil {
		return err
	}
	if err = validateProxmoxConfig(&config.Proxmox); err != nil {
		return err
	}
	return nil
}

func loadConfig(path string) (clustercheckConfig, error) {
	var (
		config clustercheckConfig
		err    error
	)

	if err = serverconfig.Read(path, &config); err != nil {
		return clustercheckConfig{}, fmt.Errorf("load configuration: %w", err)
	}
	if err = validateConfig(&config); err != nil {
		return clustercheckConfig{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return config, nil
}
