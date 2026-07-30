package main

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config is the live, hot-reloadable knob store — Gate #0 of the design: NO behaviour is hardcoded,
// every component reads its knobs from here, and any value can change at runtime with no code edit
// and no redeploy. Knobs are scoped fractally (person > team > org, most-specific wins). In Phase 0
// the store is in-memory + settable via the API; a production backing table hot-reloads into it.
type Config struct {
	mu   sync.RWMutex
	vals map[string]string // key = scope + "\x00" + name
	log  []ConfigChange    // append-only audit trail (see Set)
}

// ConfigChange is one audited knob write: who changed what, from what, when. Gate #0 requires that
// every config change be "validated, versioned, and audited (who/what/when — same append-only
// discipline as the memory itself)", and the reason is the same reason the memory is append-only:
// config IS behaviour here. A knob write can silence recall entirely (raise the precision floor), or
// un-bound the priors, or disable forgetting — with no code change, no deploy, and until now no
// record. "Why did it stop surfacing anything last Tuesday?" was unanswerable.
type ConfigChange struct {
	At    time.Time `json:"at"`
	Scope string    `json:"scope"`
	Key   string    `json:"key"`
	From  string    `json:"from"` // "" when the knob was previously unset (running on its default)
	To    string    `json:"to"`
	By    string    `json:"by"`
}

// NewConfig returns an empty Config; every getter falls back to the caller's default, so the engine
// runs zero-config out of the box (convention over configuration).
func NewConfig() *Config { return &Config{vals: map[string]string{}} }

func ckey(scope, name string) string { return scope + "\x00" + name }

// Set writes a knob at a scope (default "org"). Takes effect immediately (live) and is audited.
func (c *Config) Set(name, value string, scope ...string) { c.SetBy(name, value, "", scope...) }

// SetBy is Set with an attributed actor. Every write lands in the append-only audit log, including
// the value it replaced — a change record without the old value cannot answer "put it back".
func (c *Config) SetBy(name, value, by string, scope ...string) {
	s := "org"
	if len(scope) > 0 && strings.TrimSpace(scope[0]) != "" {
		s = strings.TrimSpace(scope[0])
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	k := ckey(s, name)

	c.log = append(c.log, ConfigChange{
		At: time.Now().UTC(), Scope: s, Key: name, From: c.vals[k], To: value, By: by,
	})

	c.vals[k] = value
}

// History returns the audit trail, newest last. Capped at `limit` most-recent entries (0 = all).
func (c *Config) History(limit int) []ConfigChange {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if limit > 0 && len(c.log) > limit {
		return append([]ConfigChange(nil), c.log[len(c.log)-limit:]...)
	}

	return append([]ConfigChange(nil), c.log...)
}

// resolve finds a knob most-specific-first across the scope chain, then falls back to "org".
func (c *Config) resolve(name string, chain []string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, s := range append(append([]string{}, chain...), "org") {
		if v, ok := c.vals[ckey(strings.TrimSpace(s), name)]; ok {
			return v, true
		}
	}

	return "", false
}

// Str returns a string knob or def. Optional chain = more-specific scopes tried first (e.g. "person:y","team:x").
func (c *Config) Str(name, def string, chain ...string) string {
	if v, ok := c.resolve(name, chain); ok {
		return v
	}

	return def
}

// F returns a float knob or def.
func (c *Config) F(name string, def float64, chain ...string) float64 {
	if v := c.Str(name, "", chain...); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}

	return def
}

// I returns an int knob or def.
func (c *Config) I(name string, def int, chain ...string) int {
	if v := c.Str(name, "", chain...); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}

	return def
}

// withOverride returns a Config that resolves exactly like c except for one shadowed key. Used by the
// re-ranking cascade to ask the cheap stage for a wider candidate set than the caller's top_k without
// mutating shared config — a live knob write here would change behaviour for every concurrent request.
func (c *Config) withOverride(name, value string) *Config {
	c.mu.RLock()
	vals := make(map[string]string, len(c.vals)+1)

	for k, v := range c.vals {
		vals[k] = v
	}

	c.mu.RUnlock()

	vals[ckey("org", name)] = value

	return &Config{vals: vals}
}
