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
	configCacheIndex map[string]*configRouteIndex
	configCacheValid bool
)

// configRouteIndex is the lookup structure for one mount. It is built once when
// the config cache is loaded, so resolving a request neither walks every config
// nor re-sorts the wildcard patterns.
type configRouteIndex struct {
	exact    map[string]int
	patterns []int // indexes into the cached config slice, most specific first
}

// invalidateAPIConfigCache drops the cached config set; every api_configs write
// calls it so the next read reloads from SQLite.
func invalidateAPIConfigCache() {
	configCacheMu.Lock()
	configCacheItems = nil
	configCacheIndex = nil
	configCacheValid = false
	configCacheMu.Unlock()
}

// enabledAPIConfigs returns the enabled config set and its per-route index,
// loading both on first use and after any invalidation. The returned slice and
// index are shared and must be treated as read-only.
func enabledAPIConfigs() ([]APIConfigRow, map[string]*configRouteIndex, error) {
	configCacheMu.RLock()
	if configCacheValid {
		items, index := configCacheItems, configCacheIndex
		configCacheMu.RUnlock()
		return items, index, nil
	}
	configCacheMu.RUnlock()

	items, err := ListEnabledAPIConfigs()
	if err != nil {
		return nil, nil, err
	}
	index := buildConfigIndex(items)
	configCacheMu.Lock()
	configCacheItems = items
	configCacheIndex = index
	configCacheValid = true
	configCacheMu.Unlock()
	return items, index, nil
}

// buildConfigIndex groups configs by mount, indexes exact names and sorts each
// wildcard list from most to least specific.
func buildConfigIndex(items []APIConfigRow) map[string]*configRouteIndex {
	index := map[string]*configRouteIndex{}
	for position := range items {
		route := normalizeConfigRoute(items[position].Route)
		entry := index[route]
		if entry == nil {
			entry = &configRouteIndex{exact: map[string]int{}}
			index[route] = entry
		}
		if util.IsWildcardName(items[position].Name) {
			entry.patterns = append(entry.patterns, position)
			continue
		}
		entry.exact[items[position].Name] = position
	}
	for _, entry := range index {
		sort.SliceStable(entry.patterns, func(a, b int) bool {
			return util.ComparePatternSpecificity(
				items[entry.patterns[a]].Name,
				items[entry.patterns[b]].Name,
			) > 0
		})
	}
	return index
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
	items, index, err := enabledAPIConfigs()
	if err != nil {
		return nil, "", err
	}
	entry := index[normalizeConfigRoute(route)]
	if entry == nil {
		return nil, "", nil
	}
	if position, ok := entry.exact[path]; ok {
		return &items[position], "", nil
	}
	for _, position := range entry.patterns {
		if rest, ok := util.MatchConfigPattern(items[position].Name, path); ok {
			return &items[position], rest, nil
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
	items, index, err := enabledAPIConfigs()
	if err != nil {
		return "", err
	}
	entry := index[normalizeConfigRoute(route)]
	if entry == nil {
		return "", nil
	}
	for _, position := range entry.patterns {
		if _, ok := util.MatchConfigPattern(items[position].Name, path+"/x"); ok {
			return items[position].Name, nil
		}
	}
	return "", nil
}
