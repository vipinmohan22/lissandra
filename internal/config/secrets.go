package config

import (
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Mask replaces secret values in API responses. A client that sends Mask
// back unchanged keeps the stored value.
const Mask = "********"

var envRef = regexp.MustCompile(`\$\{(env|file):([^}]+)\}`)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ExpandEnv replaces ${env:NAME} references with the value of the
// environment variable NAME (or, when NAME is unset and NAME_FILE is set,
// the contents of that file), and ${file:/path} references with the
// contents of that file. File contents have trailing newlines trimmed and
// are empty if the file can't be read. This lets secrets and addresses live
// in Nomad Variables (or Vault) and reach Laya through the job's template
// block instead of the config file; files can change while Laya runs.
func ExpandEnv(s string) string {
	if s == "" {
		return s
	}
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		sub := envRef.FindStringSubmatch(m)
		if sub[1] == "file" {
			return readRef(sub[2])
		}
		if !envName.MatchString(sub[2]) {
			return m
		}
		if v, ok := os.LookupEnv(sub[2]); ok {
			return v
		}
		if path := os.Getenv(sub[2] + "_FILE"); path != "" {
			return readRef(path)
		}
		return ""
	})
}

func readRef(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\r\n")
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

// MaskURLs hides passwords embedded in a comma-separated URL list.
func MaskURLs(list string) string {
	parts := strings.Split(list, ",")
	for i, p := range parts {
		if u, err := url.Parse(strings.TrimSpace(p)); err == nil && u.User != nil {
			if _, has := u.User.Password(); has {
				parts[i] = strings.Replace(p, strings.TrimSpace(p), u.Redacted(), 1)
			}
		}
	}
	return strings.Join(parts, ",")
}

// RestoreURLs keeps old when the client sent back its masked form.
func RestoreURLs(newV, old string) string {
	if newV != old && newV == MaskURLs(old) {
		return old
	}
	return newV
}

var sensitiveHeader = regexp.MustCompile(`(?i)auth|key|token|secret|pass|cookie`)

// MaskSource returns s with credentials hidden.
func MaskSource(s Source) Source {
	if s.Pull == nil {
		return s
	}
	p := *s.Pull
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
func RestoreSource(s *Source, old Source) {
	if s.Pull == nil || old.Pull == nil {
		return
	}
	s.Pull.Auth.Token = Restore(s.Pull.Auth.Token, old.Pull.Auth.Token)
	s.Pull.Auth.Password = Restore(s.Pull.Auth.Password, old.Pull.Auth.Password)
	s.Pull.Auth.HeaderValue = Restore(s.Pull.Auth.HeaderValue, old.Pull.Auth.HeaderValue)
	for k, v := range s.Pull.Headers {
		s.Pull.Headers[k] = Restore(v, old.Pull.Headers[k])
	}
}
