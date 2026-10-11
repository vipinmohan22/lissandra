package config

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Mask replaces secret values in API responses. A client that sends Mask
// back unchanged keeps the stored value.
const Mask = "********"

var envRef = regexp.MustCompile(`\$\{(env|file):([^}]+)\}`)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ExpandEnv replaces ${env:NAME} references with the value of the
// environment variable NAME (or, when NAME is unset and NAME_FILE is set,
// the contents of that file), and ${file:/path} references with the
// contents of that file, which must lie in one of FileRefDirs. File
// contents have trailing newlines trimmed and are empty if the file can't
// be read, isn't a regular file or is over 64 KiB. This lets secrets and
// addresses live in Nomad Variables (or Vault) and reach Laya through the
// job's template block instead of the config file; files can change while
// Laya runs.
func ExpandEnv(s string) string {
	if s == "" {
		return s
	}
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		sub := envRef.FindStringSubmatch(m)
		if sub[1] == "file" {
			path, ok := resolveFileRef(sub[2])
			if !ok {
				return ""
			}
			return readRef(path)
		}
		if !envName.MatchString(sub[2]) {
			return m
		}
		if v := os.Getenv(sub[2]); v != "" {
			return v
		}
		// NAME_FILE is set by the operator, not through the API, so it may
		// name any file.
		if path := os.Getenv(sub[2] + "_FILE"); path != "" {
			return readRef(path)
		}
		return ""
	})
}

// maxRefSize bounds what a file reference reads.
const maxRefSize = 64 << 10

// readRef reads a referenced file. Anything but a regular file (a FIFO or
// device would block or never end) reads as empty.
func readRef(path string) string {
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRefSize+1))
	if err != nil || len(b) > maxRefSize {
		return ""
	}
	return strings.TrimRight(string(b), "\r\n")
}

var (
	refMu       sync.RWMutex
	refDirs     = defaultFileRefDirs()
	refAnywhere bool
)

// defaultFileRefDirs are Nomad's secrets and task directories, Docker's
// /run/secrets, and those listed in LAYA_FILE_REF_DIRS.
func defaultFileRefDirs() []string {
	dirs := []string{os.Getenv("NOMAD_SECRETS_DIR"), os.Getenv("NOMAD_TASK_DIR"), "/run/secrets"}
	dirs = append(dirs, filepath.SplitList(os.Getenv("LAYA_FILE_REF_DIRS"))...)
	var out []string
	for _, d := range dirs {
		if d = strings.TrimSpace(d); d != "" && filepath.IsAbs(d) {
			out = append(out, filepath.Clean(d))
		}
	}
	return out
}

// FileRefDirs returns the directories ${file:} references may read from,
// so the API can't be used to read any file Laya can.
func FileRefDirs() []string {
	refMu.RLock()
	defer refMu.RUnlock()
	return slices.Clone(refDirs)
}

// SetFileRefDirs replaces FileRefDirs.
func SetFileRefDirs(dirs []string) {
	refMu.Lock()
	defer refMu.Unlock()
	refDirs = slices.Clone(dirs)
}

// AllowFileRefsAnywhere lets ${file:} references read any file: for the
// analyze command, which reads its user's own files.
func AllowFileRefsAnywhere() {
	refMu.Lock()
	defer refMu.Unlock()
	refAnywhere = true
}

// refPolicy returns whether references may read any file, and the
// allowed directories.
func refPolicy() (bool, []string) {
	refMu.RLock()
	defer refMu.RUnlock()
	return refAnywhere, refDirs
}

// inDirs reports whether path lies in one of dirs (as given, or with their
// symlinks resolved).
func inDirs(path string, dirs []string) bool {
	within := func(d string) bool {
		rel, err := filepath.Rel(d, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	for _, d := range dirs {
		if within(d) {
			return true
		}
		if r, err := filepath.EvalSymlinks(d); err == nil && within(r) {
			return true
		}
	}
	return false
}

// resolveFileRef returns the file a ${file:} reference reads: path with its
// symlinks resolved, when that lies in one of FileRefDirs. The resolved
// path is what is read, so a symlink and ".." can't lead elsewhere.
func resolveFileRef(path string) (string, bool) {
	anywhere, dirs := refPolicy()
	if anywhere {
		return path, true
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", false
	}
	r, err := filepath.EvalSymlinks(path)
	if err != nil || !inDirs(r, dirs) {
		return "", false
	}
	return r, true
}

// fileRefAllowed reports whether a reference to path may be saved: an
// absolute, clean path in one of FileRefDirs (after resolving symlinks, when
// the file exists already; resolveFileRef checks again when it is read).
func fileRefAllowed(path string) bool {
	anywhere, dirs := refPolicy()
	if anywhere {
		return true
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return inDirs(r, dirs)
	}
	return inDirs(path, dirs)
}

// CheckRefs reports a ${file:} reference in s outside FileRefDirs.
func CheckRefs(s string) error {
	for _, m := range envRef.FindAllStringSubmatch(s, -1) {
		if m[1] == "file" && !fileRefAllowed(m[2]) {
			return refError(m[2])
		}
	}
	return nil
}

func refError(path string) error {
	dirs := FileRefDirs()
	if len(dirs) == 0 {
		return fmt.Errorf("${file:%s}: no directory is allowed for file references; list them in LAYA_FILE_REF_DIRS", path)
	}
	return fmt.Errorf("${file:%s}: only files under %s can be referenced, by an absolute path without \"..\" (LAYA_FILE_REF_DIRS adds directories)", path, strings.Join(dirs, ", "))
}

// fileRefs lists the paths of the ${file:} references in the values of c
// that are expanded: notifier settings, pull-source credentials, headers
// and body, and the engine's URL and key.
func fileRefs(c *Config) []string {
	vals := []string{c.Settings.Engine.URL, c.Settings.Engine.APIKey}
	for _, n := range c.Notifiers {
		for _, v := range n.Settings {
			vals = append(vals, v)
		}
	}
	for _, src := range c.Sources {
		if p := src.Pull; p != nil {
			vals = append(vals, p.Body, p.Auth.Token, p.Auth.Username, p.Auth.Password, p.Auth.HeaderValue)
			for _, v := range p.Headers {
				vals = append(vals, v)
			}
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, v := range vals {
		for _, m := range envRef.FindAllStringSubmatch(v, -1) {
			if m[1] == "file" && !seen[m[2]] {
				seen[m[2]] = true
				out = append(out, m[2])
			}
		}
	}
	return out
}

// CheckNewRefs reports a ${file:} reference outside FileRefDirs that next
// has and prev doesn't. References are checked as they are added, so a
// config saved before the check existed (or an operator's seed file) still
// loads; such references read as empty (see FileRefWarnings).
func CheckNewRefs(prev, next *Config) error {
	old := map[string]bool{}
	for _, p := range fileRefs(prev) {
		old[p] = true
	}
	for _, p := range fileRefs(next) {
		if !old[p] && !fileRefAllowed(p) {
			return refError(p)
		}
	}
	return nil
}

// FileRefWarnings describes the ${file:} references in c that can't be
// read, being outside FileRefDirs: they read as empty.
func FileRefWarnings(c *Config) []string {
	var out []string
	for _, p := range fileRefs(c) {
		if !fileRefAllowed(p) {
			out = append(out, refError(p).Error()+"; it reads as empty")
		}
	}
	return out
}

// HasRef reports whether s contains an ${env:…} or ${file:…} reference.
func HasRef(s string) bool { return envRef.MatchString(s) }

// MaskValue hides v unless it is empty or an ${env:...} reference, which is
// not itself secret.
func MaskValue(v string) string {
	if v == "" || (envRef.MatchString(v) && envRef.ReplaceAllString(v, "") == "") {
		return v
	}
	return Mask
}

// Restore returns old when the client sent the mask back unchanged.
func Restore(newV, old string) string {
	if newV == Mask {
		return old
	}
	return newV
}

// urlMask replaces credentials in URLs.
const urlMask = "xxxxx"

// userinfo splits a URL into what comes before its user part, the user
// part (without the "@") and the rest. The URL text is otherwise left as
// it is: pull URLs carry placeholders a parse and re-encode would escape.
func userinfo(raw string) (before, user, rest string, ok bool) {
	i := strings.Index(raw, "://")
	if i < 0 {
		return "", "", "", false
	}
	start := i + 3
	end := len(raw)
	if j := strings.IndexAny(raw[start:], "/?#"); j >= 0 {
		end = start + j
	}
	at := strings.LastIndex(raw[start:end], "@")
	if at < 0 {
		return "", "", "", false
	}
	return raw[:start], raw[start : start+at], raw[start+at+1:], true
}

// maskUser hides the password of user:password, or a user alone (often a
// token).
func maskUser(user string) string {
	if name, _, ok := strings.Cut(user, ":"); ok {
		return name + ":" + urlMask
	}
	return urlMask
}

// MaskURLs hides credentials (user:password, or a user alone) embedded in
// a comma-separated URL list.
func MaskURLs(list string) string {
	parts := strings.Split(list, ",")
	for i, p := range parts {
		parts[i] = MaskURL(p)
	}
	return strings.Join(parts, ",")
}

// MaskURL is MaskURLs for a single URL, whose password may hold a comma.
func MaskURL(raw string) string {
	t := strings.TrimSpace(raw)
	if before, user, rest, ok := userinfo(t); ok {
		return strings.Replace(raw, t, before+maskUser(user)+"@"+rest, 1)
	}
	return raw
}

// RestoreURLs puts back the credentials of each entry in newV that the
// client left masked, from the entry in old for the same address (or the
// same host on another port, or the only credentials old has). It fails
// when there is nothing to restore a masked entry from.
func RestoreURLs(newV, old string) (string, error) {
	if !strings.Contains(newV, urlMask) {
		return newV, nil
	}
	parts, err := restoreParts(strings.Split(newV, ","), strings.Split(old, ","))
	return strings.Join(parts, ","), err
}

// RestoreURL is RestoreURLs for a single URL, whose password may hold a
// comma.
func RestoreURL(newV, old string) (string, error) {
	if !strings.Contains(newV, urlMask) {
		return newV, nil
	}
	parts, err := restoreParts([]string{newV}, []string{old})
	return parts[0], err
}

func restoreParts(parts, oldParts []string) ([]string, error) {
	type cred struct{ scheme, host, hostname, user string }
	parse := func(raw string) (cred, bool) {
		_, user, _, ok := userinfo(raw)
		u, err := url.Parse(raw)
		if !ok || err != nil {
			return cred{}, false
		}
		return cred{u.Scheme, u.Host, u.Hostname(), user}, true
	}
	var olds []cred
	for _, p := range oldParts {
		if c, ok := parse(strings.TrimSpace(p)); ok {
			olds = append(olds, c)
		}
	}
	for i, p := range parts {
		t := strings.TrimSpace(p)
		before, user, rest, ok := userinfo(t)
		if !ok || user != maskUser(user) {
			continue // no credentials, or typed in full
		}
		n, _ := parse(t)
		same := func(o cred) bool { return o.scheme == n.scheme && maskUser(o.user) == user }
		var from *cred
		for _, match := range []func(o cred) bool{
			func(o cred) bool { return o.host == n.host },
			func(o cred) bool { return o.hostname == n.hostname },
			func(o cred) bool { return true },
		} {
			for j := range olds {
				o := &olds[j]
				if !same(*o) || !match(*o) {
					continue
				}
				if from != nil && from.user != o.user {
					from = nil // different credentials: which is meant?
					break
				}
				from = o
			}
			if from != nil {
				break
			}
		}
		if from == nil {
			return parts, fmt.Errorf("%s: the credentials are masked; type them again", before+user+"@"+rest)
		}
		parts[i] = strings.Replace(p, t, before+from.user+"@"+rest, 1)
	}
	return parts, nil
}

var sensitiveHeader = regexp.MustCompile(`(?i)auth|key|token|secret|pass|cookie`)

// MaskSource returns s with credentials hidden.
func MaskSource(s Source) Source {
	if s.Pull == nil {
		return s
	}
	p := *s.Pull
	p.URL = MaskURL(p.URL)
	p.Auth.Token = MaskValue(p.Auth.Token)
	p.Auth.Password = MaskValue(p.Auth.Password)
	p.Auth.HeaderValue = MaskValue(p.Auth.HeaderValue)
	if len(p.Headers) > 0 {
		h := make(map[string]string, len(p.Headers))
		for k, v := range p.Headers {
			if sensitiveHeader.MatchString(k) {
				v = MaskValue(v)
			}
			h[k] = v
		}
		p.Headers = h
	}
	s.Pull = &p
	return s
}

// RestoreSource puts back credentials the client left masked.
func RestoreSource(s *Source, old Source) error {
	if s.Pull == nil || old.Pull == nil {
		return nil
	}
	u, err := RestoreURL(s.Pull.URL, old.Pull.URL)
	if err != nil {
		return fmt.Errorf("pull url: %w", err)
	}
	s.Pull.URL = u
	s.Pull.Auth.Token = Restore(s.Pull.Auth.Token, old.Pull.Auth.Token)
	s.Pull.Auth.Password = Restore(s.Pull.Auth.Password, old.Pull.Auth.Password)
	s.Pull.Auth.HeaderValue = Restore(s.Pull.Auth.HeaderValue, old.Pull.Auth.HeaderValue)
	for k, v := range s.Pull.Headers {
		s.Pull.Headers[k] = Restore(v, old.Pull.Headers[k])
	}
	return nil
}
