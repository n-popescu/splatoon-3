package matchmaking

import (
	"encoding/json"
	"log"
	"os"
	"sync"
)

// ConfigRules describes how many players a matchmaking config wants.
//
// Splatoon 3 asks for a named matchmaking config (a resource under
// tenants/…/matchmakingConfigs/) and the SERVER decides what that config means:
// how many players a match needs, how many it accepts, whether the room is
// public. Retail Nintendo configures this server-side, so nothing in the game
// tells us — which is exactly why it is an operator-editable file rather than a
// constant somebody has to recompile.
type ConfigRules struct {
	// Name is the config as the game names it. Matching is done on the last
	// path segment too, so "regular" matches "tenants/…/matchmakingConfigs/regular".
	Name string `json:"name"`
	// MinPlayers is the number of players below which a room is not started.
	MinPlayers int `json:"min_players"`
	// MaxPlayers is the room limit.
	MaxPlayers int `json:"max_players"`
	// Comment is free-form documentation kept in the file for whoever edits it.
	Comment string `json:"comment,omitempty"`
}

// ConfigSet holds the known configs plus the fallback rules.
type ConfigSet struct {
	mu       sync.RWMutex
	byName   map[string]ConfigRules
	fallback ConfigRules
	// unknown remembers the configs we already warned about, so the log lists
	// each unknown config once instead of once per ticket.
	unknown map[string]bool
}

// NewConfigSet builds a set with the given defaults.
func NewConfigSet(defaultMin, defaultMax int) *ConfigSet {
	if defaultMin <= 0 {
		defaultMin = 2
	}
	if defaultMax < defaultMin {
		defaultMax = defaultMin
	}
	return &ConfigSet{
		byName:   map[string]ConfigRules{},
		unknown:  map[string]bool{},
		fallback: ConfigRules{Name: "(default)", MinPlayers: defaultMin, MaxPlayers: defaultMax},
	}
}

// LoadFile merges a JSON file of ConfigRules into the set. A missing file is not
// an error: the defaults then apply to everything, which is a working (if
// coarse) configuration.
func (c *ConfigSet) LoadFile(path string) error {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("[mm] no matchmaking config file at %s; using min=%d max=%d for every config",
				path, c.fallback.MinPlayers, c.fallback.MaxPlayers)
			return nil
		}
		return err
	}
	rules, err := parseConfigFile(b)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range rules {
		if r.MinPlayers <= 0 {
			r.MinPlayers = c.fallback.MinPlayers
		}
		if r.MaxPlayers < r.MinPlayers {
			r.MaxPlayers = r.MinPlayers
		}
		c.byName[lastSegment(r.Name)] = r
		log.Printf("[mm] config %q: %d..%d players%s", r.Name, r.MinPlayers, r.MaxPlayers, comment(r))
	}
	return nil
}

// parseConfigFile accepts either a bare JSON array of rules or an object with a
// "configs" array (so the shipped example can carry an explanatory comment
// without pretending to be a config).
func parseConfigFile(b []byte) ([]ConfigRules, error) {
	var asArray []ConfigRules
	if err := json.Unmarshal(b, &asArray); err == nil {
		return asArray, nil
	}
	var asObject struct {
		Configs []ConfigRules `json:"configs"`
	}
	if err := json.Unmarshal(b, &asObject); err != nil {
		return nil, err
	}
	return asObject.Configs, nil
}

// For returns the rules for a config name, falling back to the defaults.
//
// An unknown config is logged ONCE per name: that log line is the list of
// matchmaking configs Splatoon 3 actually uses, which is the information an
// operator needs to fill the file in.
func (c *ConfigSet) For(name string) ConfigRules {
	key := lastSegment(name)
	c.mu.RLock()
	r, ok := c.byName[key]
	c.mu.RUnlock()
	if ok {
		return r
	}
	c.mu.Lock()
	if !c.unknown[key] {
		c.unknown[key] = true
		log.Printf("[mm] matchmaking config %q is not described in the config file; using min=%d max=%d",
			name, c.fallback.MinPlayers, c.fallback.MaxPlayers)
	}
	c.mu.Unlock()
	out := c.fallback
	out.Name = name
	return out
}

func comment(r ConfigRules) string {
	if r.Comment == "" {
		return ""
	}
	return " — " + r.Comment
}

// lastSegment returns the final path element of a resource name.
func lastSegment(s string) string {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return s[i+1:]
		}
	}
	return s
}
