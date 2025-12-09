package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// NetworkProfile defines how a logical profile maps to AWS ENI configuration.
type NetworkProfile struct {
	SubnetID          string   `yaml:"subnetId" json:"subnetId"`
	SecurityGroupIDs  []string `yaml:"securityGroupIds" json:"securityGroupIds"`
	Description       string   `yaml:"description" json:"description"`
	PreferredENICount int      `yaml:"preferredEniCount" json:"preferredEniCount"`
}

// Config represents the agent runtime configuration.
type Config struct {
	NetworkProfiles map[string]NetworkProfile `yaml:"networkProfiles" json:"networkProfiles"`
}

// Load reads YAML configuration from the given path.
func Load(path string) (*Config, error) {
	if path == "" {
		return &Config{NetworkProfiles: map[string]NetworkProfile{}}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if cfg.NetworkProfiles == nil {
		cfg.NetworkProfiles = map[string]NetworkProfile{}
	}
	return &cfg, nil
}

// ResolveProfile fetches a profile by key.
func (c *Config) ResolveProfile(name string) (NetworkProfile, bool) {
	if c == nil {
		return NetworkProfile{}, false
	}
	p, ok := c.NetworkProfiles[name]
	return p, ok
}
