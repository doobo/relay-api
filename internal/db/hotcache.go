package db

import (
	"sync"
	"sync/atomic"
	"time"
)

// AIResourceGeneration increments on every write to the tables the /v1 hot path
// resolves against (providers, models, model_routes). Higher-level caches such
// as router.Resolver compare this value and drop their entries when it moves,
// so a config change is visible on the very next request without a restart.
var aiResourceGeneration atomic.Uint64

// AIResourceGeneration returns the current generation of the AI routing tables.
func AIResourceGeneration() uint64 { return aiResourceGeneration.Load() }

// invalidateAIResourceCache marks the cached routing data stale. Called by
// every providers/models/model_routes write.
func invalidateAIResourceCache() { aiResourceGeneration.Add(1) }

// API-key cache. Auth runs on every /v1 and /open request while keys only
// change through the admin API, so the hashed-key lookup is cached by positive
// result (an unknown key is never cached, keeping arbitrary bearer strings from
// growing the map) and dropped on any api_keys write.
var (
	apiKeyCacheMu sync.RWMutex
	apiKeyCache   = make(map[string]APIKeyRow)
)

func cachedAPIKey(hash string) (APIKeyRow, bool) {
	apiKeyCacheMu.RLock()
	key, ok := apiKeyCache[hash]
	apiKeyCacheMu.RUnlock()
	return key, ok
}

func storeAPIKey(hash string, key APIKeyRow) {
	apiKeyCacheMu.Lock()
	apiKeyCache[hash] = key
	apiKeyCacheMu.Unlock()
}

func invalidateAPIKeyCache() {
	apiKeyCacheMu.Lock()
	apiKeyCache = make(map[string]APIKeyRow)
	apiKeyCacheMu.Unlock()
}

// Quota counters. The SQL aggregate (APIKeyQuotaUsage) is a scan over the
// key's usage_logs rows; running it on every request is the most expensive
// thing on the auth path once quotas are configured. Counters are loaded once
// from SQLite and then advanced in memory as usage records are recorded.
type quotaCounter struct {
	mu sync.Mutex

	loaded bool

	dayStart   int64
	weekStart  int64
	monthStart int64

	dayReq, dayTok     int
	weekReq, weekTok   int
	monthReq, monthTok int

	// Records recorded before the first load finishes must not be lost when
	// the loaded totals overwrite the counters.
	pendingReq int
	pendingTok int
}

var (
	quotaMu       sync.RWMutex
	quotaCounters = make(map[int64]*quotaCounter)
)

// quotaTrackedKeys records whether a key has any quota limits configured. It is
// populated when the key row is loaded (a key-cache miss), so the relay path can
// skip the quota maps entirely for keys that are not quota-limited. A missing
// entry means "unknown" and is treated as tracked, preserving the behaviour for
// callers that record usage without going through API-key auth.
var quotaTrackedKeys sync.Map // map[int64]bool

// rememberQuotaTracking records whether a key needs quota-counter maintenance.
// When the setting flips (for example an admin adds a quota to a key that had
// none), the in-memory counter is dropped so the next check reloads the real
// totals from SQLite instead of undercounting usage recorded while disabled.
func rememberQuotaTracking(key APIKeyRow) {
	tracked := key.HasQuotaLimits()
	previous, known := quotaTrackedKeys.Swap(key.ID, tracked)
	if known && previous.(bool) != tracked {
		forgetQuotaCounter(key.ID)
	}
}

// clearQuotaTracking drops the hints after the database handle changes; they
// are repopulated the next time each key is loaded.
func clearQuotaTracking() {
	quotaTrackedKeys.Range(func(key, _ any) bool {
		quotaTrackedKeys.Delete(key)
		return true
	})
}

func quotaEntry(keyID int64) *quotaCounter {
	quotaMu.RLock()
	counter := quotaCounters[keyID]
	quotaMu.RUnlock()
	if counter != nil {
		return counter
	}
	quotaMu.Lock()
	defer quotaMu.Unlock()
	if counter = quotaCounters[keyID]; counter == nil {
		counter = &quotaCounter{}
		quotaCounters[keyID] = counter
	}
	return counter
}

func forgetQuotaCounter(keyID int64) {
	quotaMu.Lock()
	delete(quotaCounters, keyID)
	quotaMu.Unlock()
}

// addAPIKeyQuotaUsage advances a key's counters when a usage record is
// recorded. It runs on the relay path (before the async insert is queued) so
// quota enforcement does not wait for the batch writer.
func addAPIKeyQuotaUsage(keyID int64, tokens int) {
	if keyID <= 0 {
		return
	}
	// Most keys have no quota configured; skip the counter bookkeeping (and its
	// per-key lock) for them. An unknown key is treated as tracked so direct
	// recording callers and tests keep the old behaviour.
	if tracked, known := quotaTrackedKeys.Load(keyID); known && !tracked.(bool) {
		return
	}
	counter := quotaEntry(keyID)
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if !counter.loaded {
		counter.pendingReq++
		counter.pendingTok += tokens
		return
	}
	counter.rollLocked(time.Now())
	counter.dayReq++
	counter.dayTok += tokens
	counter.weekReq++
	counter.weekTok += tokens
	counter.monthReq++
	counter.monthTok += tokens
}

// rollLocked resets the counters for periods that have rolled over. Caller
// holds counter.mu.
func (c *quotaCounter) rollLocked(now time.Time) {
	day, week, month := startOfDayMs(now), startOfWeekMs(now), startOfMonthMs(now)
	if c.dayStart != day {
		c.dayReq, c.dayTok = 0, 0
		c.dayStart = day
	}
	if c.weekStart != week {
		c.weekReq, c.weekTok = 0, 0
		c.weekStart = week
	}
	if c.monthStart != month {
		c.monthReq, c.monthTok = 0, 0
		c.monthStart = month
	}
}

// APIKeyQuotaUsageCached is APIKeyQuotaUsage backed by in-memory counters. The
// first call for a key loads the SQLite aggregate; every later call is O(1)
// and includes usage recorded since that load.
func APIKeyQuotaUsageCached(keyID int64) (QuotaUsage, error) {
	counter := quotaEntry(keyID)
	counter.mu.Lock()
	defer counter.mu.Unlock()

	now := time.Now()
	if !counter.loaded {
		usage, err := APIKeyQuotaUsage(keyID)
		if err != nil {
			return QuotaUsage{}, err
		}
		counter.dayReq, counter.dayTok = usage.DayRequests, usage.DayTokens
		counter.weekReq, counter.weekTok = usage.WeekRequests, usage.WeekTokens
		counter.monthReq, counter.monthTok = usage.MonthRequests, usage.MonthTokens
		counter.dayStart = startOfDayMs(now)
		counter.weekStart = startOfWeekMs(now)
		counter.monthStart = startOfMonthMs(now)
		counter.loaded = true
		counter.dayReq += counter.pendingReq
		counter.dayTok += counter.pendingTok
		counter.weekReq += counter.pendingReq
		counter.weekTok += counter.pendingTok
		counter.monthReq += counter.pendingReq
		counter.monthTok += counter.pendingTok
		counter.pendingReq, counter.pendingTok = 0, 0
	} else {
		counter.rollLocked(now)
	}

	return QuotaUsage{
		DayRequests:   counter.dayReq,
		DayTokens:     counter.dayTok,
		WeekRequests:  counter.weekReq,
		WeekTokens:    counter.weekTok,
		MonthRequests: counter.monthReq,
		MonthTokens:   counter.monthTok,
	}, nil
}

// resetHotCaches drops every cache whose contents belong to a database handle.
// It runs on Open/Close so a fresh handle can never serve rows from the
// previous one, and it bumps the generation so resolvers held by the caller
// drop their own entries too.
func resetHotCaches() {
	aiResourceGeneration.Add(1)
	invalidateAPIKeyCache()
	clearQuotaTracking()
	quotaMu.Lock()
	quotaCounters = make(map[int64]*quotaCounter)
	quotaMu.Unlock()
	resetLiveStats()
	resetConfigStatsCache()
}
