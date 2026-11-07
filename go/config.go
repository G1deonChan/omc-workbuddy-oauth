package main

import (
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// defaultAppVersion and defaultCLIVersion are the desktop build and the CLI it
// bundles that the plugin presents itself as. WorkBuddy refuses a chat whose
// User-Agent does not start "WorkBuddy/<version>" (error 10085), so both are
// configuration: a vendor build bump is a config change, not a new release.
const (
	defaultAppVersion = "5.9.3"
	defaultCLIVersion = "2.165.0"
)

type config struct {
	// Site is the fallback build for a login request that names no entry.
	Site string `yaml:"site"`
	// MaxConcurrency bounds simultaneous requests per credential.
	MaxConcurrency int `yaml:"max_concurrency"`
	// AppVersion is the WorkBuddy desktop version the client claims.
	AppVersion string `yaml:"app_version"`
	// CLIVersion is the CLI version the client claims.
	CLIVersion string `yaml:"cli_version"`
}

var configMu sync.RWMutex
var currentConfig = defaults()

func defaults() config {
	return config{
		Site:           siteWorkBuddy,
		MaxConcurrency: 3,
		AppVersion:     defaultAppVersion,
		CLIVersion:     defaultCLIVersion,
	}
}

func getConfig() config { configMu.RLock(); defer configMu.RUnlock(); return currentConfig }

func configure(raw []byte) error {
	c := defaults()
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("invalid plugin configuration")
		}
	}
	c.Site = strings.ToLower(strings.TrimSpace(c.Site))
	if c.Site == "" {
		c.Site = siteWorkBuddy
	}
	if _, ok := siteFor(c.Site); !ok {
		return fmt.Errorf("invalid workbuddy plugin configuration: site must be workbuddy or workbuddy-ai")
	}
	if c.MaxConcurrency < 1 || c.MaxConcurrency > 100 {
		return fmt.Errorf("invalid workbuddy plugin configuration: max_concurrency must be 1-100")
	}
	var err error
	if c.AppVersion, err = cleanVersion(c.AppVersion, "app_version"); err != nil {
		return err
	}
	if c.CLIVersion, err = cleanVersion(c.CLIVersion, "cli_version"); err != nil {
		return err
	}
	configMu.Lock()
	currentConfig = c
	configMu.Unlock()
	return nil
}

// cleanVersion keeps a version string usable in a User-Agent: digits and dots,
// short, and never blank.
func cleanVersion(value, field string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("invalid workbuddy plugin configuration: %s must not be empty", field)
	}
	if len(v) > 32 {
		return "", fmt.Errorf("invalid workbuddy plugin configuration: %s must be at most 32 characters", field)
	}
	for _, r := range v {
		if (r < '0' || r > '9') && r != '.' && r != '-' && r != '+' && r != '_' {
			return "", fmt.Errorf("invalid workbuddy plugin configuration: %s must be a version string", field)
		}
	}
	return v, nil
}

// configuredSpec adopts the pinned client identity, so the build a config names
// is what every request claims.
func configuredSpec(id string) (siteSpec, bool) {
	s, ok := siteFor(id)
	if !ok {
		return s, false
	}
	cfg := getConfig()
	s.App = cfg.AppVersion
	s.CLI = cfg.CLIVersion
	return s, true
}
