package inventory

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type VMConfig struct {
	Name           string `yaml:"name"`
	GuestID        string `yaml:"guestId"`
	Disk           string `yaml:"disk"`
	DiskSizeGB     int64  `yaml:"diskSizeGB"`     // disk capacity in GB (default: 10)
	MemoryMB       int32  `yaml:"memoryMB"`
	NumCPUs        int32  `yaml:"numCPUs"`
	Firmware       string `yaml:"firmware"`       // "bios" or "efi" (default: "bios")
	DiskController string `yaml:"diskController"` // "pvscsi" or "lsilogic-sas" (default: "pvscsi")
}

type Config struct {
	Datacenter string     `yaml:"datacenter"`
	Cluster    string     `yaml:"cluster"`
	Host       string     `yaml:"host"`
	Datastore  string     `yaml:"datastore"`
	Network    string     `yaml:"network"`
	Folder     string     `yaml:"folder"`
	VMs        []VMConfig `yaml:"vms"`

	// Runtime settings (not from YAML)
	ListenAddr   string `yaml:"-"`
	DiskPath     string `yaml:"-"`
	ExternalHost string `yaml:"-"`
	Username     string `yaml:"-"`
	Password     string `yaml:"-"`
	UserSuffix   string `yaml:"-"` // appended to VM names (e.g. "-user1")
	ESXMode      bool   `yaml:"-"` // use ESX (HostAgent) model instead of VPX
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.applyEnvOverrides()
	cfg.applyDefaults()

	return cfg, nil
}

func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("FVMW_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if v := os.Getenv("FVMW_DISK_PATH"); v != "" {
		c.DiskPath = v
	}
	if v := os.Getenv("FVMW_EXTERNAL_HOST"); v != "" {
		c.ExternalHost = v
	}
	if v := os.Getenv("FVMW_HOST"); v != "" {
		c.Host = v
	}
	if v := os.Getenv("FVMW_USERNAME"); v != "" {
		c.Username = v
	}
	if v := os.Getenv("FVMW_PASSWORD"); v != "" {
		c.Password = v
	}
	if v := os.Getenv("FVMW_USER_SUFFIX"); v != "" {
		c.UserSuffix = v
	}
	if os.Getenv("FVMW_ESX_MODE") != "" {
		c.ESXMode = true
	}
}

func (c *Config) applyDefaults() {
	if c.ListenAddr == "" {
		c.ListenAddr = ":8080"
	}
	if c.DiskPath == "" {
		c.DiskPath = "/disks"
	}
	if c.Username == "" {
		c.Username = "administrator@vsphere.local"
	}
	if c.Password == "" {
		c.Password = "password"
	}
}
