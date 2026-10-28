package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Reasoning replay.
//
// Some reasoning-capable upstreams require the reasoning text of earlier
// assistant turns to be sent back on every later request. DeepSeek V4 is the
// usual offender:
//
//	The `reasoning_content` in the thinking mode must be passed back to the API.
//	The `content[].thinking` in the thinking mode must be passed back to the API.
//
// OpenAI-format clients rarely echo it (OpenCode and most agents drop reasoning
// from the history they resend), so the second turn of every conversation fails
// with a 400 even though the first turn worked. The gateway therefore remembers
// the reasoning it relayed and re-attaches it when the client omits it.
//
// Entries are keyed by provider, model alias, resolved client session and the
// assistant turn itself (its text plus tool-call ids), so reasoning cannot leak
// into another conversation or another model.
const (
	// reasoningCacheEntries bounds the cache. One entry is one assistant turn,
	// so this covers many concurrent agent sessions.
	reasoningCacheEntries = 2048
	// reasoningMaxBytes caps a single entry; reasoning is usually a few KB.
	reasoningMaxBytes = 64 << 10
	// reasoningTTL drops turns that are too old to still be in a conversation.
	reasoningTTL = 6 * time.Hour
)

type reasoningEntry struct {
	reasoning string
	storedAt  time.Time
}

type reasoningCacheState struct {
	mu    sync.Mutex
	order []string // insertion order, for FIFO eviction
	items map[string]reasoningEntry
	count atomic.Int64
}

var reasoningCache = &reasoningCacheState{items: make(map[string]reasoningEntry)}

// HasReasoning reports whether anything is cached, letting the request path
// skip the JSON decode entirely on a cold cache.
func HasReasoning() bool { return reasoningCache.count.Load() > 0 }

// ResetReasoningCache drops every entry (tests, and useful if an admin ever
// needs a clean slate).
func ResetReasoningCache() {
	reasoningCache.mu.Lock()
	reasoningCache.order = nil
	reasoningCache.items = make(map[string]reasoningEntry)
	reasoningCache.mu.Unlock()
	reasoningCache.count.Store(0)
}

// ReasoningKey fingerprints one assistant turn. The NUL separators keep
// concatenated fields from colliding ("ab"+"c" vs "a"+"bc").
func ReasoningKey(provider, model, session, text string, toolIDs []string) string {
	hash := sha256.New()
	for _, part := range []string{provider, model, session, text, strings.Join(toolIDs, ",")} {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// RememberReasoning stores the reasoning text produced for one assistant turn.
func RememberReasoning(provider, model, session, text string, toolIDs []string, reasoning string) {
	reasoning = strings.TrimSpace(reasoning)
	if reasoning == "" {
		return
	}
	if len(reasoning) > reasoningMaxBytes {
		reasoning = reasoning[:reasoningMaxBytes]
	}
	reasoningCache.put(ReasoningKey(provider, model, session, text, toolIDs), reasoning)
}

// RecallReasoning returns the reasoning text remembered for one assistant turn.
func RecallReasoning(provider, model, session, text string, toolIDs []string) (string, bool) {
	return reasoningCache.get(ReasoningKey(provider, model, session, text, toolIDs))
}

func (c *reasoningCacheState) put(key, reasoning string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; !exists {
		c.order = append(c.order, key)
	}
	c.items[key] = reasoningEntry{reasoning: reasoning, storedAt: time.Now()}
	c.evictLocked(time.Now())
	c.count.Store(int64(len(c.items)))
}

func (c *reasoningCacheState) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok {
		return "", false
	}
	if time.Since(entry.storedAt) > reasoningTTL {
		delete(c.items, key)
		c.count.Store(int64(len(c.items)))
		return "", false
	}
	return entry.reasoning, true
}

// evictLocked drops expired entries and then the oldest ones over the cap.
// Caller holds c.mu.
func (c *reasoningCacheState) evictLocked(now time.Time) {
	kept := c.order[:0]
	for _, key := range c.order {
		entry, ok := c.items[key]
		if !ok {
			continue
		}
		if now.Sub(entry.storedAt) > reasoningTTL {
			delete(c.items, key)
			continue
		}
		kept = append(kept, key)
	}
	c.order = kept
	for len(c.order) > reasoningCacheEntries {
		delete(c.items, c.order[0])
		c.order = c.order[1:]
	}
}
