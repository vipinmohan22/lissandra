package config

import (
	"os"
	"regexp"
)

// Mask replaces secret values in API responses. A client that sends Mask
// back unchanged keeps the stored value.
const Mask = "********"

var envRef = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv replaces ${env:NAME} references with the value of the
// environment variable NAME. This lets secrets live in Nomad Variables (or
// Vault) and reach Laya through the job's template block instead of being
// stored in the config file.
func ExpandEnv(s string) string {
	if s == "" {
		return s
	}
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(envRef.FindStringSubmatch(m)[1])
	})
}

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
