package db

import (
	"sort"

	"relay-api/internal/util"
)

// normalizeConfigRoute maps a stored route to a known mount ("open"/"free").
func normalizeConfigRoute(route string) string {
	if route == "free" {
		return "free"
	}
	return "open"
}

// GetAPIConfigByName loads an enabled config by exact name (nil when missing).
func GetAPIConfigByName(name string) (*APIConfigRow, error) {
	return scanAPIConfig(Get().QueryRow(`SELECT `+apiConfigColumns+` FROM api_configs WHERE name = ? AND enabled = 1`, name))
}

// ListEnabledAPIConfigs returns every enabled config.
func ListEnabledAPIConfigs() ([]APIConfigRow, error) {
	rows, err := Get().Query(`SELECT ` + apiConfigColumns + ` FROM api_configs WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var configs []APIConfigRow
	for rows.Next() {
		config, err := scanAPIConfig(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, *config)
	}
	return configs, rows.Err()
}

// ResolveAPIConfig maps a request path (below the forwarding mount) to a config:
// exact names first, then wildcard patterns from most to least specific. Only
// configs registered on `route` are considered. The returned rest is the path
// tail the pattern consumed (empty for exact names).
func ResolveAPIConfig(path, route string) (*APIConfigRow, string, error) {
	exact, err := GetAPIConfigByName(path)
	if err != nil {
		return nil, "", err
	}
	if exact != nil && normalizeConfigRoute(exact.Route) == route {
		return exact, "", nil
	}

	configs, err := ListEnabledAPIConfigs()
	if err != nil {
		return nil, "", err
	}
	patterns := make([]APIConfigRow, 0, len(configs))
	for _, config := range configs {
		if normalizeConfigRoute(config.Route) == route && util.IsWildcardName(config.Name) {
			patterns = append(patterns, config)
		}
	}
	sort.SliceStable(patterns, func(i, j int) bool {
		return util.ComparePatternSpecificity(patterns[i].Name, patterns[j].Name) > 0
	})
	for index := range patterns {
		if rest, ok := util.MatchConfigPattern(patterns[index].Name, path); ok {
			config := patterns[index]
			return &config, rest, nil
		}
	}
	return nil, "", nil
}

// FindWildcardConfigBelow explains an unmatched path when a wildcard config is
// one segment short of it (e.g. `/free/stream` vs a `stream/**` config).
func FindWildcardConfigBelow(path, route string) (string, error) {
	if path == "" {
		return "", nil
	}
	configs, err := ListEnabledAPIConfigs()
	if err != nil {
		return "", err
	}
	for _, config := range configs {
		if normalizeConfigRoute(config.Route) != route || !util.IsWildcardName(config.Name) {
			continue
		}
		if _, ok := util.MatchConfigPattern(config.Name, path+"/x"); ok {
			return config.Name, nil
		}
	}
	return "", nil
}
