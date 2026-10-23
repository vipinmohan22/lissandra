package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/vipinmohan22/layalogs/internal/atomicfile"
)

// Store owns the configuration and persists every change atomically to a
// JSON file. Readers get deep copies, so callers may mutate what they get.
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  *Config
	subs []func(*Config)
}

// Open loads the config at path. When the file doesn't exist it is created
// from seedPath (if set and present) or from built-in defaults.
func Open(path, seedPath string) (*Store, error) {
	s := &Store{path: path}
	c, err := Load(path)
	switch {
	case err == nil:
		s.cfg = c
		return s, nil
	case errors.Is(err, os.ErrNotExist):
		c, err := initial(seedPath)
		if err != nil {
			return nil, err
		}
		s.cfg = c
		if err := s.save(c); err != nil {
			return nil, err
		}
		return s, nil
	default:
		return nil, err
	}
}

// initial is the config a first start writes. Only one built from the
// defaults offers the setup wizard: a seed's operator configured Laya as
// code, so a seed gets it only by asking with its own "onboarding" key.
func initial(seedPath string) (*Config, error) {
	now := time.Now().UTC().Truncate(time.Second)
	if seedPath != "" {
		data, err := os.ReadFile(seedPath)
		if err == nil {
			var c Config
			if err := Decode(data, &c); err != nil {
				return nil, fmt.Errorf("parse seed %s: %w", seedPath, err)
			}
			if err := Normalize(&c); err != nil {
				return nil, fmt.Errorf("invalid seed %s: %w", seedPath, err)
			}
			if o := c.Onboarding; o != nil {
				// A seed gives steps without dates.
				if o.StartedAt == nil {
					o.StartedAt = &now
				}
				for id, st := range o.Steps {
					if st.At.IsZero() {
						st.At = now
						o.Steps[id] = st
					}
				}
			}
			return &c, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	c := Defaults()
	c.Onboarding = &Onboarding{Status: OnboardingPending, StartedAt: &now}
	if err := Normalize(c); err != nil {
		return nil, err
	}
	return c, nil
}

// ErrSave marks an Update that failed to write the config file: a server
// fault, not a problem with the change.
var ErrSave = errors.New("saving the configuration failed")

// Decode parses a config document into c. A document without engine
// settings (written before the engine existed) gets the engine's defaults
// rather than zero values, such as a confidence threshold of 0; one that has
// them keeps exactly what it says, except that rule learning is on when it
// isn't mentioned.
func Decode(data []byte, c *Config) error {
	var probe struct {
		Settings struct {
			Engine json.RawMessage `json:"engine"`
		} `json:"settings"`
	}
	*c = Config{}
	if json.Unmarshal(data, &probe) == nil && (probe.Settings.Engine == nil || string(probe.Settings.Engine) == "null") {
		c.Settings.Engine = Defaults().Settings.Engine
	} else {
		// Engine settings written before rule learning existed get it on;
		// a document that says learn_rules keeps what it says.
		c.Settings.Engine.LearnRules = true
	}
	return json.Unmarshal(data, c)
}

// Load reads and validates the config at path without creating or
// modifying anything. It is meant for offline tools such as `laya analyze`.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := Decode(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := Normalize(&c); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

// Get returns a deep copy of the current configuration.
func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.cfg)
}

// Update applies fn to a copy of the configuration, validates and persists
// the result, then notifies subscribers. If fn or validation fails nothing
// changes. An update that changes only the setup state is saved quietly:
// the version stays and subscribers aren't called, so recording a wizard
// step doesn't reconfigure the pipeline, notifiers, engine and pollers.
func (s *Store) Update(fn func(c *Config) error) error {
	s.mu.Lock()
	next := clone(s.cfg)
	if err := fn(next); err != nil {
		s.mu.Unlock()
		return err
	}
	if err := Normalize(next); err != nil {
		s.mu.Unlock()
		return err
	}
	if err := CheckNewRefs(s.cfg, next); err != nil {
		s.mu.Unlock()
		return err
	}
	onboardingOnly := !jsonEqual(next.Onboarding, s.cfg.Onboarding) && jsonEqual(withoutOnboarding(next), withoutOnboarding(s.cfg))
	if !onboardingOnly {
		next.Version = s.cfg.Version + 1
	}
	if err := s.save(next); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrSave, err)
	}
	s.cfg = next
	if onboardingOnly {
		s.mu.Unlock()
		return nil
	}
	subs := append([]func(*Config){}, s.subs...)
	s.mu.Unlock()

	for _, fn := range subs {
		fn(clone(next))
	}
	return nil
}

// Subscribe registers fn to be called with the new config after every
// successful Update. It is also called once immediately.
func (s *Store) Subscribe(fn func(*Config)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	c := clone(s.cfg)
	s.mu.Unlock()
	fn(c)
}

func (s *Store) save(c *Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path, data, 0o600)
}

// withoutOnboarding is c without its setup state, to tell whether an
// update changed anything else.
func withoutOnboarding(c *Config) *Config {
	cc := *c
	cc.Onboarding = nil
	return &cc
}

func jsonEqual(a, b any) bool {
	x, err := json.Marshal(a)
	if err != nil {
		return false
	}
	y, err := json.Marshal(b)
	return err == nil && bytes.Equal(x, y)
}

func clone(c *Config) *Config {
	data, err := json.Marshal(c)
	if err != nil {
		panic(fmt.Sprintf("config: marshal: %v", err))
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		panic(fmt.Sprintf("config: unmarshal: %v", err))
	}
	return &out
}
