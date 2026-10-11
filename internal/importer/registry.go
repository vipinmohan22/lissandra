package importer

import (
	"errors"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vipinmohan22/layalogs/internal/model"
)

// Registry errors.
var (
	ErrBusy      = errors.New("another log file is being analyzed; try again when it finishes")
	ErrRunning   = errors.New("the analysis is still running")
	ErrNotFound  = errors.New("import not found")
	ErrBadID     = errors.New("import id must be 16 lowercase hex characters")
	ErrDuplicate = errors.New("an import with this id already exists")
)

var validID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// RegistryConfig bounds the registry.
type RegistryConfig struct {
	Keep          int    // finished imports kept (oldest dropped first)
	Concurrency   int    // imports allowed at once
	Limits        Limits // per-import retained events
	RetainedBytes int64  // retained-event bytes across all finished imports
}

// Registry keeps recent imports in memory.
type Registry struct {
	cfg     RegistryConfig
	mu      sync.Mutex
	imports []*Import // oldest first
	active  int

	Started, Succeeded, Failed, Events atomic.Uint64
}

// NewRegistry returns a registry; zero fields in cfg take defaults.
func NewRegistry(cfg RegistryConfig) *Registry {
	if cfg.Keep <= 0 {
		cfg.Keep = 5
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.Limits == (Limits{}) {
		cfg.Limits = DefaultLimits
	}
	if cfg.RetainedBytes <= 0 {
		cfg.RetainedBytes = 64 << 20
	}
	return &Registry{cfg: cfg}
}

// Config returns the registry's effective limits.
func (reg *Registry) Config() RegistryConfig { return reg.cfg }

// Begin registers a new import. id may be empty (one is generated) or a
// client-chosen 16-hex-character id, which lets a client follow an import
// while it is still uploading. Pair every successful Begin with Done.
func (reg *Registry) Begin(id, fileName string, now time.Time) (*Import, error) {
	if id == "" {
		id = model.NewID()
	} else if !validID.MatchString(id) {
		return nil, ErrBadID
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, im := range reg.imports {
		if im.ID() == id {
			return nil, ErrDuplicate
		}
	}
	if reg.active >= reg.cfg.Concurrency {
		return nil, ErrBusy
	}
	reg.active++
	im := newImport(id, fileName, reg.cfg.Limits, now)
	reg.imports = append(reg.imports, im)
	reg.evictLocked()
	reg.Started.Add(1)
	return im, nil
}

// Done releases the slot taken by Begin, updates counters and enforces the
// memory budget.
func (reg *Registry) Done(im *Import) {
	rep := im.Report(false)
	reg.mu.Lock()
	reg.active--
	reg.evictLocked()
	reg.mu.Unlock()
	if rep.Status == StatusFailed {
		reg.Failed.Add(1)
	} else {
		reg.Succeeded.Add(1)
	}
	reg.Events.Add(uint64(rep.Events))
}

// evictLocked drops the oldest finished imports beyond Keep, then frees the
// retained events of the oldest finished imports while their total exceeds
// the byte budget (their reports stay).
func (reg *Registry) evictLocked() {
	for len(reg.imports) > reg.cfg.Keep {
		idx := -1
		for i, im := range reg.imports {
			if !im.Running() {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		reg.imports = append(reg.imports[:idx], reg.imports[idx+1:]...)
	}
	var total int64
	for _, im := range reg.imports {
		if !im.Running() {
			total += im.retainedBytes()
		}
	}
	for _, im := range reg.imports { // oldest first
		if total <= reg.cfg.RetainedBytes {
			break
		}
		if !im.Running() {
			total -= im.dropEvents()
		}
	}
}

// List returns summaries, newest first.
func (reg *Registry) List() []Report {
	reg.mu.Lock()
	list := append([]*Import(nil), reg.imports...)
	reg.mu.Unlock()
	out := make([]Report, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		out = append(out, list[i].Report(false))
	}
	return out
}

// Get returns an import by ID.
func (reg *Registry) Get(id string) (*Import, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, im := range reg.imports {
		if im.ID() == id {
			return im, true
		}
	}
	return nil, false
}

// Delete removes a finished import.
func (reg *Registry) Delete(id string) error {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for i, im := range reg.imports {
		if im.ID() == id {
			if im.Running() {
				return ErrRunning
			}
			reg.imports = append(reg.imports[:i], reg.imports[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// Active reports imports in progress.
func (reg *Registry) Active() int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.active
}
