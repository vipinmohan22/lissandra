package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

func initial(seedPath string) (*Config, error) {
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
			return &c, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	c := Defaults()
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
// them keeps exactly what it says.
func Decode(data []byte, c *Config) error {
	var probe struct {
		Settings struct {
			Engine json.RawMessage `json:"engine"`
		} `json:"settings"`
	}
	*c = Config{}
	if json.Unmarshal(data, &probe) == nil && (probe.Settings.Engine == nil || string(probe.Settings.Engine) == "null") {
		c.Settings.Engine = Defaults().Settings.Engine
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
// changes.
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
	next.Version = s.cfg.Version + 1
	if err := s.save(next); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrSave, err)
	}
	s.cfg = next
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
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
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
