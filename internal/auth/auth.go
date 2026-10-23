// Package auth holds Laya's admin credential: one bearer token for the UI
// and API. It comes from LAYA_ADMIN_TOKEN, or from a token set in Laya and
// stored as a keyed hash in admin-token.json, or else is generated for one
// run. The token is an API key, not a password: it is checked on every
// request, so the hash is fast (HMAC-SHA256) and the token's strength comes
// from it being random.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vipinmohan22/layalogs/internal/atomicfile"
	"github.com/vipinmohan22/layalogs/internal/model"
)

// Source is where the active admin token comes from.
type Source string

// Token sources, in order of precedence.
const (
	SourceEnv       Source = "env"       // LAYA_ADMIN_TOKEN; nothing else is accepted
	SourceStored    Source = "stored"    // set in Laya, kept in admin-token.json
	SourceGenerated Source = "generated" // random, for this run only (logged)
)

// Errors from Rotate and DeleteStored.
var (
	ErrEnvManaged = errors.New("the admin token is set by LAYA_ADMIN_TOKEN; change it there")
	ErrWeakToken  = errors.New("the admin token is too weak") // wrapped with the rule that failed
	ErrSameToken  = errors.New("the new admin token is the one in use")
	ErrStale      = errors.New("the admin token was changed by another request")
	ErrNotEnv     = errors.New("a stored token can only be deleted while LAYA_ADMIN_TOKEN is set")
	ErrNoStored   = errors.New("no admin token is stored in Laya")
)

// Status describes the active admin token, never the token itself.
type Status struct {
	Source        Source     `json:"source"`
	Changeable    bool       `json:"changeable"`     // false for env
	Persistent    bool       `json:"persistent"`     // false for generated
	SetAt         *time.Time `json:"set_at"`         // stored only
	StoredIgnored bool       `json:"stored_ignored"` // env set and a file exists
	StoredSetAt   *time.Time `json:"stored_set_at"`
	File          string     `json:"file"` // path of admin-token.json ("" for Static)
}

const (
	fileVersion = 1
	scheme      = "hmac-sha256"
	keySize     = 32
)

// file is admin-token.json.
type file struct {
	Version int       `json:"version"`
	Scheme  string    `json:"scheme"`
	Key     []byte    `json:"key"`    // base64
	Digest  []byte    `json:"digest"` // base64 of HMAC-SHA256(key, token)
	SetAt   time.Time `json:"set_at"`
}

// verifier checks a token against its HMAC under a random key. Every
// source is kept this way, so no plaintext token stays in memory.
type verifier struct {
	key    []byte
	digest [sha256.Size]byte
}

func newVerifier(token string) verifier {
	v := verifier{key: make([]byte, keySize)}
	if _, err := rand.Read(v.key); err != nil {
		panic(fmt.Sprintf("auth: random key: %v", err))
	}
	v.digest = v.sum(token)
	return v
}

func (v verifier) sum(token string) [sha256.Size]byte {
	m := hmac.New(sha256.New, v.key)
	m.Write([]byte(token))
	var out [sha256.Size]byte
	m.Sum(out[:0])
	return out
}

// match compares fixed-size digests, so neither the token's length nor
// its contents leak through timing.
func (v verifier) match(token string) bool {
	got := v.sum(token)
	return subtle.ConstantTimeCompare(got[:], v.digest[:]) == 1
}

type state struct {
	v             verifier
	source        Source
	setAt         *time.Time
	storedIgnored bool
	storedSetAt   *time.Time
}

// Store is the active admin credential. Check is lock-free; Rotate and
// DeleteStored are serialized.
type Store struct {
	path   string
	maxLen int
	mu     sync.Mutex
	cur    atomic.Pointer[state]
}

// Open picks the credential: LAYA_ADMIN_TOKEN (envToken) if set, else the
// token stored at path, else a token generated for this run, which it
// returns for the caller to log. A stored file that can't be used fails
// Open unless envToken is set: falling back to a generated token would let
// anyone who reads the log in, and lock out whoever holds the stored one.
func Open(path, envToken string, log *slog.Logger) (s *Store, generated string, err error) {
	s = &Store{path: path, maxLen: max(256, len(envToken))}
	stored, err := readFile(path)
	if envToken != "" {
		st := &state{v: newVerifier(envToken), source: SourceEnv}
		switch {
		case err != nil:
			log.Warn("admin-token.json can't be used; it is ignored while LAYA_ADMIN_TOKEN is set", "file", path, "err", err)
			st.storedIgnored = true
		case stored != nil:
			st.storedIgnored, st.storedSetAt = true, &stored.SetAt
		}
		s.cur.Store(st)
		return s, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w; fix or delete it, or set LAYA_ADMIN_TOKEN", path, err)
	}
	if stored != nil {
		// Anyone who can write the data directory controls config.json too,
		// and volumes (fsGroup, NFS, SMB, Docker Desktop) may report modes
		// chmod can't change: warn, don't refuse.
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
			log.Warn(fmt.Sprintf("admin-token.json is accessible to other users (mode %#o); Laya writes it with mode 0600", fi.Mode().Perm()), "file", path)
		}
		v := verifier{key: stored.Key}
		copy(v.digest[:], stored.Digest)
		s.cur.Store(&state{v: v, source: SourceStored, setAt: &stored.SetAt})
		return s, "", nil
	}
	generated = model.NewToken()
	s.cur.Store(&state{v: newVerifier(generated), source: SourceGenerated})
	return s, generated, nil
}

// readFile reads admin-token.json: nil without an error when there is none.
func readFile(path string) (*file, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	switch {
	case f.Version != fileVersion:
		return nil, fmt.Errorf("unknown version %d", f.Version)
	case f.Scheme != scheme:
		return nil, fmt.Errorf("unknown scheme %q", f.Scheme)
	case len(f.Key) != keySize:
		return nil, fmt.Errorf("the key is %d bytes, not %d", len(f.Key), keySize)
	case len(f.Digest) != sha256.Size:
		return nil, fmt.Errorf("the digest is %d bytes, not %d", len(f.Digest), sha256.Size)
	}
	return &f, nil
}

// Static is a store for one token, as if set by LAYA_ADMIN_TOKEN, with no
// file: for tests, and for a server built without a store.
func Static(token string) *Store {
	s := &Store{maxLen: max(256, len(token))}
	s.cur.Store(&state{v: newVerifier(token), source: SourceEnv})
	return s
}

// Check reports whether presented is the admin token. Overlong input is
// refused before hashing.
func (s *Store) Check(presented string) bool {
	return len(presented) > 0 && len(presented) <= s.maxLen && s.cur.Load().v.match(presented)
}

// Status describes the active token.
func (s *Store) Status() Status {
	st := s.cur.Load()
	return Status{
		Source:        st.source,
		Changeable:    st.source != SourceEnv,
		Persistent:    st.source != SourceGenerated,
		SetAt:         copyTime(st.setAt),
		StoredIgnored: st.storedIgnored,
		StoredSetAt:   copyTime(st.storedSetAt),
		File:          s.path,
	}
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// Rotate replaces the admin token with next and stores it. presented must
// be the token in use: of two rotations made with the same token, only the
// first succeeds. The file is written before the new token is used, so a
// failed write leaves the old one working; once Rotate returns, the old
// token (a generated one included) is refused.
func (s *Store) Rotate(presented, next string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.cur.Load()
	if cur.source == SourceEnv {
		return Status{}, ErrEnvManaged
	}
	if !cur.v.match(presented) {
		return Status{}, ErrStale
	}
	if err := ValidateToken(next); err != nil {
		return Status{}, err
	}
	if cur.v.match(next) {
		return Status{}, ErrSameToken
	}
	v := newVerifier(next)
	now := time.Now().UTC().Truncate(time.Second)
	data, err := json.MarshalIndent(file{Version: fileVersion, Scheme: scheme, Key: v.key, Digest: v.digest[:], SetAt: now}, "", "  ")
	if err != nil {
		return Status{}, err
	}
	if err := atomicfile.Write(s.path, append(data, '\n'), 0o600); err != nil {
		return Status{}, fmt.Errorf("save the admin token: %w", err)
	}
	s.cur.Store(&state{v: v, source: SourceStored, setAt: &now})
	return s.Status(), nil
}

// DeleteStored removes a stored token that LAYA_ADMIN_TOKEN overrides, so
// removing the variable later can't bring an old token back to life.
func (s *Store) DeleteStored() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.cur.Load()
	if cur.source != SourceEnv {
		return ErrNotEnv
	}
	if s.path == "" {
		return ErrNoStored
	}
	if err := os.Remove(s.path); errors.Is(err, os.ErrNotExist) {
		return ErrNoStored
	} else if err != nil {
		return err
	}
	next := *cur
	next.storedIgnored, next.storedSetAt = false, nil
	s.cur.Store(&next)
	return nil
}

// ValidateToken checks a token about to be stored. The rules catch
// mistakes such as a short or repetitive value; they can't make a guessable
// token strong, which is why the UI generates one. LAYA_ADMIN_TOKEN is not
// checked, so existing deployments keep working.
func ValidateToken(t string) error {
	weak := func(rule string) error { return fmt.Errorf("%w: %s", ErrWeakToken, rule) }
	if len(t) < 32 || len(t) > 256 {
		return weak("use 32 to 256 characters")
	}
	counts := map[byte]int{}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c < 0x21 || c > 0x7e {
			// Browsers can't send other bytes in a header.
			return weak("use printable ASCII characters, without spaces")
		}
		counts[c]++
	}
	if len(counts) < 10 {
		return weak("use at least 10 different characters")
	}
	for _, n := range counts {
		if n*4 > len(t) {
			return weak("no character may make up more than a quarter of it")
		}
	}
	return nil
}
