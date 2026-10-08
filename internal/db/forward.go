package db

import (
	"sort"
	"sync"

	"relay-api/internal/util"
)

// normalizeConfigRoute maps a stored route to a known mount ("open"/"free").
func normalizeConfigRoute(route string) string {
	if route == "free" {
		return "free"
	}
	return "open"
}

// API-config cache. Route resolution runs on every relay request while the
// config set only changes through the admin API, so the enabled configs are
// kept in memory and re-read from SQLite only after a write. This takes the
// per-request config query off the /free, /open and /v1 hot paths.
var (
	configCacheMu    sync.RWMutex
	configCacheItems []APIConfigRow
	configCacheValid bool
)

// invalidateAPIConfigCache drops the cached config set; every api_configs write
// calls it so the next read reloads from SQLite.
func invalidateAPIConfigCache() {
	configCacheMu.Lock()
	configCacheItems = nil
	configCacheValid = false
	configCacheMu.Unlock()
}

// enabledAPIConfigs returns the enabled config set, loading it on first use and
// after any invalidation. The returned slice is shared and must be treated as
// read-only.
func enabledAPIConfigs() ([]APIConfigRow, error) {
	configCacheMu.RLock()
	if configCacheValid {
		items := configCacheItems
		configCacheMu.RUnlock()
		return items, nil
	}
	configCacheMu.RUnlock()

	items, err := ListEnabledAPIConfigs()
	if err != nil {
		return nil, err
	}
	configCacheMu.Lock()
	configCacheItems = items
	configCacheValid = true
	configCacheMu.Unlock()
	return items, nil
}

// ListEnabledAPIConfigs returns every enabled config, straight from SQLite.
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
// tail the pattern consumed (empty for exact names). The config set comes from
// the in-memory cache, so a warm resolver performs no query at all.
func ResolveAPIConfig(path, route string) (*APIConfigRow, string, error) {
	configs, err := enabledAPIConfigs()
	if err != nil {
		return nil, "", err
	}

	var exact *APIConfigRow
	patterns := make([]APIConfigRow, 0, len(configs))
	for index := range configs {
		config := configs[index]
		if normalizeConfigRoute(config.Route) != route {
			continue
		}
		if util.IsWildcardName(config.Name) {
			patterns = append(patterns, config)
			continue
		}
		if config.Name == path {
			exact = &config
		}
	}
	if exact != nil {
		return exact, "", nil
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
	configs, err := enabledAPIConfigs()
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
