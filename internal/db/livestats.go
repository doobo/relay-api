package db

import (
	"sort"
	"sync"
	"time"
)

// liveStats keeps today's dashboard counters in memory. The dashboard polls
// /admin/stats every 30 seconds; recomputing COUNT/SUM/AVG over usage_logs on
// every poll scans millions of rows (one day at 2000 req/min is ~2.9M rows) and
// holds a read connection for hundreds of milliseconds, starving auth and admin
// session lookups. The writer feeds these counters once per inserted record, so
// the endpoint becomes O(1).
//
// The counters are seeded from SQLite when the database is opened and reset at
// local midnight. Usage rows recorded before the process started are included
// in the seed; rows deleted by retention are not subtracted (today's window is
// short enough that this does not matter).
type liveStatsState struct {
	mu      sync.Mutex
	loaded  bool
	dayFrom int64

	requests     int
	tokens       int
	errors       int
	latencySum   int64
	latencyCount int64

	models    map[string]*ModelStat
	providers map[string]*ProviderStat
}

var liveStats liveStatsState

// resetLiveStats drops the counters; the next Open reseeds them from SQLite.
// Called from resetHotCaches so a fresh database handle never reuses the
// previous handle's numbers.
func resetLiveStats() {
	liveStats.mu.Lock()
	liveStats.loaded = false
	liveStats.dayFrom = 0
	liveStats.requests = 0
	liveStats.tokens = 0
	liveStats.errors = 0
	liveStats.latencySum = 0
	liveStats.latencyCount = 0
	liveStats.models = nil
	liveStats.providers = nil
	liveStats.mu.Unlock()
}

// initLiveStats seeds the in-memory counters from SQLite. It runs during Open,
// before the server accepts requests, so there is no concurrent note().
func initLiveStats() error {
	today, err := queryTodayAggregate()
	if err != nil {
		return err
	}
	models, err := modelStatsTodaySQL()
	if err != nil {
		return err
	}
	providers, err := providerStatsTodaySQL()
	if err != nil {
		return err
	}

	modelMap := make(map[string]*ModelStat, len(models))
	for index := range models {
		stat := models[index]
		modelMap[stat.Model] = &stat
	}
	providerMap := make(map[string]*ProviderStat, len(providers))
	for index := range providers {
		stat := providers[index]
		providerMap[stat.Provider] = &stat
	}

	liveStats.mu.Lock()
	liveStats.loaded = true
	liveStats.dayFrom = StartOfDayMS(time.Now())
	liveStats.requests = today.Requests
	liveStats.tokens = today.Tokens
	liveStats.errors = today.Errors
	liveStats.latencySum = today.LatencySum
	liveStats.latencyCount = today.LatencyCount
	liveStats.models = modelMap
	liveStats.providers = providerMap
	liveStats.mu.Unlock()
	return nil
}

// note adds one persisted usage row to the live counters. It is called from the
// usage writer after a successful insert, not from the request path.
func (s *liveStatsState) note(record UsageRecord, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		return
	}
	s.rollLocked(now)

	total := usageTotal(record)
	failed := record.Status == nil || *record.Status >= 400

	s.requests++
	s.tokens += total
	if failed {
		s.errors++
	}
	if record.LatencyMs != nil {
		s.latencySum += *record.LatencyMs
		s.latencyCount++
	}

	if record.Kind != "ai" {
		return
	}
	if record.Model != nil {
		stat := s.models[*record.Model]
		if stat == nil {
			stat = &ModelStat{Model: *record.Model}
			s.models[*record.Model] = stat
		}
		stat.Requests++
		stat.Tokens += total
	}
	if record.Provider != nil {
		stat := s.providers[*record.Provider]
		if stat == nil {
			stat = &ProviderStat{Provider: *record.Provider}
			s.providers[*record.Provider] = stat
		}
		stat.Requests++
		if failed {
			stat.Errors++
		}
	}
}

// rollLocked clears the day when local midnight passed. Caller holds s.mu.
func (s *liveStatsState) rollLocked(now time.Time) {
	day := StartOfDayMS(now)
	if s.dayFrom == day {
		return
	}
	s.dayFrom = day
	s.requests = 0
	s.tokens = 0
	s.errors = 0
	s.latencySum = 0
	s.latencyCount = 0
	s.models = make(map[string]*ModelStat)
	s.providers = make(map[string]*ProviderStat)
}

// liveTodayStats returns the cached counters, or false when they are not
// loaded (the caller then falls back to SQL).
func liveTodayStats() (TodayStats, bool) {
	liveStats.mu.Lock()
	defer liveStats.mu.Unlock()
	if !liveStats.loaded {
		return TodayStats{}, false
	}
	liveStats.rollLocked(time.Now())

	stats := TodayStats{
		Requests: liveStats.requests,
		Tokens:   liveStats.tokens,
		Errors:   liveStats.errors,
	}
	if liveStats.latencyCount > 0 {
		stats.AvgLatencyMs = float64(liveStats.latencySum) / float64(liveStats.latencyCount)
	}
	return stats, true
}

// liveModelStatsToday returns a copy of today's per-model counters, busiest
// first, capped at the same 20 rows as the SQL query.
func liveModelStatsToday() ([]ModelStat, bool) {
	liveStats.mu.Lock()
	defer liveStats.mu.Unlock()
	if !liveStats.loaded {
		return nil, false
	}
	liveStats.rollLocked(time.Now())

	out := make([]ModelStat, 0, len(liveStats.models))
	for _, stat := range liveStats.models {
		out = append(out, *stat)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Requests != out[b].Requests {
			return out[a].Requests > out[b].Requests
		}
		return out[a].Model < out[b].Model
	})
	if len(out) > 20 {
		out = out[:20]
	}
	return out, true
}

// liveProviderStatsToday returns a copy of today's per-provider counters.
func liveProviderStatsToday() ([]ProviderStat, bool) {
	liveStats.mu.Lock()
	defer liveStats.mu.Unlock()
	if !liveStats.loaded {
		return nil, false
	}
	liveStats.rollLocked(time.Now())

	out := make([]ProviderStat, 0, len(liveStats.providers))
	for _, stat := range liveStats.providers {
		out = append(out, *stat)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Requests != out[b].Requests {
			return out[a].Requests > out[b].Requests
		}
		return out[a].Provider < out[b].Provider
	})
	if len(out) > 20 {
		out = out[:20]
	}
	return out, true
}
