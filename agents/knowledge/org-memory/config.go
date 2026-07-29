package main

import (
	"strconv"
	"strings"
	"sync"
)

// Config is the live, hot-reloadable knob store — Gate #0 of the design: NO behaviour is hardcoded,
// every component reads its knobs from here, and any value can change at runtime with no code edit
// and no redeploy. Knobs are scoped fractally (person > team > org, most-specific wins). In Phase 0
// the store is in-memory + settable via the API; a production backing table hot-reloads into it.
type Config struct {
	mu   sync.RWMutex
	vals map[string]string // key = scope + "\x00" + name
}

// NewConfig returns an empty Config; every getter falls back to the caller's default, so the engine
// runs zero-config out of the box (convention over configuration).
func NewConfig() *Config { return &Config{vals: map[string]string{}} }

func ckey(scope, name string) string { return scope + "\x00" + name }

// Set writes a knob at a scope (default "org"). Takes effect immediately (live).
func (c *Config) Set(name, value string, scope ...string) {
	s := "org"
	if len(scope) > 0 && strings.TrimSpace(scope[0]) != "" {
		s = strings.TrimSpace(scope[0])
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.vals[ckey(s, name)] = value
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
