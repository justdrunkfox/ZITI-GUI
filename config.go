package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

type Config struct {
	IdentityDir string          `json:"identity_dir"`
	Service     string          `json:"service"`
	ZitiBin     string          `json:"ziti_bin"`
	PollSec     int             `json:"poll_sec"`
	Disabled    map[string]bool `json:"disabled"` // имя идентичности -> выключена
}

func configDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "ziti-gui")
}

func configPath() string { return filepath.Join(configDir(), "config.json") }

func defaultIdentityDir() string {
	if _, err := os.Stat("/opt/openziti/etc/identities"); err == nil {
		return "/opt/openziti/etc/identities"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "ziti", "identities")
}

func loadConfig() *Config {
	cfg := &Config{
		IdentityDir: defaultIdentityDir(),
		Service:     "ziti-edge-tunnel.service",
		ZitiBin:     "",
		PollSec:     3,
		Disabled:    map[string]bool{},
	}
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	var saved Config
	if json.Unmarshal(data, &saved) == nil {
		if saved.IdentityDir != "" {
			cfg.IdentityDir = saved.IdentityDir
		}
		if saved.Service != "" {
			cfg.Service = saved.Service
		}
		if saved.ZitiBin != "" {
			cfg.ZitiBin = saved.ZitiBin
		}
		if saved.PollSec > 0 {
			cfg.PollSec = saved.PollSec
		}
		if saved.Disabled != nil {
			cfg.Disabled = saved.Disabled
		}
	}
	return cfg
}

func (c *Config) save() {
	os.MkdirAll(configDir(), 0o755)
	// не храним записи, которых больше нет смысла хранить
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(configPath(), data, 0o600)
}

func sortedNames[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
