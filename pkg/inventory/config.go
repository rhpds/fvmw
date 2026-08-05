package inventory

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type VMConfig struct {
	Name           string `yaml:"name"`
	GuestID        string `yaml:"guestId"`
	Disk           string `yaml:"disk"`
	DiskSizeGB     int64  `yaml:"diskSizeGB"` // disk capacity in GB (default: 10)
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
	NumUsers     int    `yaml:"-"` // generated users count via FVMW_NUM_USERS
	UserSuffix   string `yaml:"-"` // appended to VM names (e.g. "-user1")
	ESXMode      bool   `yaml:"-"` // use ESX (HostAgent) model instead of VPX
	ESXiPrefix   string `yaml:"-"` // optional hostname prefix that forces HostAgent apiType
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
	if v, ok := os.LookupEnv("FVMW_NUM_USERS"); ok {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			c.NumUsers = n
		}
	}
	if v := os.Getenv("FVMW_USER_SUFFIX"); v != "" {
		c.UserSuffix = v
	}
	if v, ok := os.LookupEnv("FVMW_ESXI_PREFIX"); ok {
		c.ESXiPrefix = v
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
	if c.NumUsers <= 0 {
		c.NumUsers = 60
	}
}

// AuthCredentials returns allowed usernames/passwords.
// In multi-user mode, generated users (user1..userN) share FVMW_PASSWORD.
// In legacy single-user mode (FVMW_USER_SUFFIX), FVMW_USERNAME/FVMW_PASSWORD
// are used.
func (c *Config) AuthCredentials() map[string]string {
	creds := make(map[string]string)
	if c.UserSuffix != "" {
		creds[c.Username] = c.Password
		return creds
	}

	for _, user := range c.generatedUsers() {
		creds[user] = c.Password
	}
	return creds
}

// VMNameSuffixes returns the suffixes used to create VMs.
// Priority:
// 1) Explicit FVMW_USER_SUFFIX (legacy single-user mode)
// 2) Derived from FVMW_NUM_USERS as -user1..-userN
func (c *Config) VMNameSuffixes() []string {
	if c.UserSuffix != "" {
		return []string{c.UserSuffix}
	}

	users := c.generatedUsers()
	suffixes := make([]string, 0, len(users))
	for _, user := range users {
		suffixes = append(suffixes, "-"+user)
	}
	return suffixes
}

// ScopeVMsByUser indicates whether REST VM listings should be filtered per user.
func (c *Config) ScopeVMsByUser() bool {
	return c.UserSuffix == "" && c.NumUsers > 0
}

func (c *Config) generatedUsers() []string {
	users := make([]string, 0, c.NumUsers)
	for i := 1; i <= c.NumUsers; i++ {
		users = append(users, "user"+strconv.Itoa(i))
	}
	return users
}
