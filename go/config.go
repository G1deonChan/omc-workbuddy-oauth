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
	// ExcludeModels are model ids this plugin never advertises. WorkBuddy's
	// product config lists models an account's plan does not include, and the
	// only way to learn that is to be refused; naming them here keeps them out
	// of the list from the first request, so CPA never routes one to this
	// credential.
	ExcludeModels []string `yaml:"exclude_models"`
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
	excluded, err := cleanModelList(c.ExcludeModels)
	if err != nil {
		return err
	}
	c.ExcludeModels = excluded
	configMu.Lock()
	currentConfig = c
	configMu.Unlock()
	return nil
}

// cleanModelList normalizes the model ids a config names: trimmed, lowercased,
// deduplicated, and kept to ids a model can actually have. A value that cannot
// be a model id is a configuration error rather than a silently ignored line.
func cleanModelList(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, raw := range values {
		for _, part := range strings.Split(raw, ",") {
			id := strings.ToLower(strings.TrimSpace(part))
			if id == "" {
				continue
			}
			if len(id) > 128 || !modelIDLike.MatchString(id) {
				return nil, fmt.Errorf("invalid workbuddy plugin configuration: exclude_models entry %q is not a model id", part)
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out, nil
}

// modelExcluded reports a model the configuration keeps out of the published
// list, whatever WorkBuddy's own config says about it.
func modelExcluded(model string) bool {
	want := strings.ToLower(strings.TrimSpace(model))
	if want == "" {
		return false
	}
	for _, id := range getConfig().ExcludeModels {
		if id == want {
			return true
		}
	}
	return false
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
