package s3site

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
)

const accessPolicyPath = ".s3site/access.json"

// AccessPolicy protects the whole site or evaluates ordered path rules.
// A rules policy defaults to deny. The first matching rule wins.
type AccessPolicy struct {
	Version int          `json:"version"`
	Auth    string       `json:"auth"`
	Allow   *Allowlist   `json:"allow,omitempty"`
	Rules   []AccessRule `json:"rules,omitempty"`
}

// Allowlist grants access when any listed identity or role matches.
// Email grants require a verified email; roles come from an operator-selected claim.
type Allowlist struct {
	Emails   []string `json:"emails,omitempty"`
	Subjects []string `json:"subjects,omitempty"`
	Roles    []string `json:"roles,omitempty"`
}

// AccessRule matches either an exact decoded path or a segment-bounded prefix.
// Exactly one of Public or Allow must grant the matching path.
type AccessRule struct {
	Path       string     `json:"path,omitempty"`
	PathPrefix string     `json:"pathPrefix,omitempty"`
	Public     bool       `json:"public,omitempty"`
	Allow      *Allowlist `json:"allow,omitempty"`
}

func (a *Allowlist) permits(s session) bool {
	if a == nil {
		return false
	}
	for _, subject := range a.Subjects {
		if subject == s.Subject {
			return true
		}
	}
	if s.EmailVerified {
		for _, email := range a.Emails {
			if strings.EqualFold(email, s.Email) {
				return true
			}
		}
	}
	for _, role := range a.Roles {
		for _, actual := range s.Roles {
			if role == actual {
				return true
			}
		}
	}
	return false
}

func (p *AccessPolicy) permits(s session) bool { return p.Allow.permits(s) }

func (p *AccessPolicy) validate() error {
	if p.Version != 1 || p.Auth != "oidc" {
		return fmt.Errorf("requires version 1 and auth oidc")
	}
	if p.Allow != nil && p.Rules != nil {
		return fmt.Errorf("use either allow or rules")
	}
	validateAllow := func(a *Allowlist) error {
		if a == nil {
			return nil
		}
		for _, email := range a.Emails {
			if strings.TrimSpace(email) != email || !strings.Contains(email, "@") {
				return fmt.Errorf("invalid email")
			}
		}
		for _, value := range append(append([]string{}, a.Subjects...), a.Roles...) {
			if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
				return fmt.Errorf("empty or padded subject/role")
			}
		}
		return nil
	}
	if err := validateAllow(p.Allow); err != nil {
		return err
	}
	for i, r := range p.Rules {
		if (r.Path == "") == (r.PathPrefix == "") {
			return fmt.Errorf("rule %d requires exactly one of path or pathPrefix", i)
		}
		match := r.Path
		if match == "" {
			match = r.PathPrefix
		}
		if !strings.HasPrefix(match, "/") || strings.ContainsAny(match, "?%#\\") || (path.Clean(match) != strings.TrimSuffix(match, "/") && match != "/") {
			return fmt.Errorf("rule %d requires a clean absolute decoded path", i)
		}
		if r.Public == (r.Allow != nil) {
			return fmt.Errorf("rule %d requires either public true or allow", i)
		}
		if err := validateAllow(r.Allow); err != nil {
			return fmt.Errorf("rule %d: %w", i, err)
		}
	}
	return nil
}

type accessDecision struct {
	grants []*Allowlist
	denied bool
}

func (d accessDecision) public() bool { return !d.denied && len(d.grants) == 0 }
func (d accessDecision) permits(s session) bool {
	if d.denied {
		return false
	}
	for _, a := range d.grants {
		if !a.permits(s) {
			return false
		}
	}
	return true
}
func (p *AccessPolicy) match(requestPath string) accessDecision {
	if p.Allow != nil {
		return accessDecision{grants: []*Allowlist{p.Allow}}
	}
	clean := path.Clean("/" + requestPath)
	for _, r := range p.Rules {
		prefix := strings.TrimSuffix(r.PathPrefix, "/")
		if (r.Path != "" && (r.Path == requestPath || path.Clean(r.Path) == clean)) || (r.PathPrefix != "" && (prefix == "" || clean == prefix || strings.HasPrefix(clean, prefix+"/"))) {
			if r.Public {
				return accessDecision{}
			}
			return accessDecision{grants: []*Allowlist{r.Allow}}
		}
	}
	return accessDecision{denied: true}
}

// Authorize both the URL and its representation: directory indexes and custom
// 404 pages must not bypass a rule protecting their direct file paths.
func (p *AccessPolicy) decision(siteFS fs.FS, requestPath string) accessDecision {
	d := p.match(requestPath)
	name := strings.TrimPrefix(path.Clean("/"+requestPath), "/")
	if name == "" {
		name = "."
	}
	effective := ""
	if info, err := fs.Stat(siteFS, name); err == nil && info.IsDir() {
		if index, indexErr := fs.Stat(siteFS, path.Join(name, "index.html")); indexErr == nil && !index.IsDir() {
			effective = "/" + strings.TrimPrefix(path.Join(name, "index.html"), "./")
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		if info, fallbackErr := fs.Stat(siteFS, "404.html"); fallbackErr == nil && !info.IsDir() {
			effective = "/404.html"
		}
	}
	if effective != "" {
		other := p.match(effective)
		d.denied = d.denied || other.denied
		d.grants = append(d.grants, other.grants...)
	}
	return d
}

func prepareSite(s *site) (*site, error) {
	f, err := s.fs.Open(accessPolicyPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.cleanup()
		return nil, fmt.Errorf("access policy: %w", err)
	}
	if err == nil {
		defer f.Close()
		// Policies are deliberately small and never parsed on the request path.
		data, readErr := io.ReadAll(io.LimitReader(f, 64*1024+1))
		if readErr != nil || len(data) > 64*1024 {
			s.cleanup()
			return nil, fmt.Errorf("access policy unreadable or exceeds 64 KiB")
		}
		var p AccessPolicy
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			s.cleanup()
			return nil, fmt.Errorf("access policy: %w", err)
		}
		if dec.Decode(new(any)) != io.EOF {
			s.cleanup()
			return nil, fmt.Errorf("access policy requires one JSON object")
		}
		if err := p.validate(); err != nil {
			s.cleanup()
			return nil, fmt.Errorf("access policy: %w", err)
		}
		s.policy = &p
	}
	s.fs = privateFS{s.fs}
	return s, nil
}

// privateFS hides the entire reserved metadata directory, including listings.
type privateFS struct{ fs.FS }

func (p privateFS) Open(name string) (fs.File, error) {
	if name == ".s3site" || strings.HasPrefix(name, ".s3site/") {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	f, err := p.FS.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.IsDir() {
		if d, ok := f.(fs.ReadDirFile); ok {
			return &privateDir{ReadDirFile: d}, nil
		}
	}

	return f, nil
}

type privateDir struct{ fs.ReadDirFile }

func (d *privateDir) ReadDir(n int) ([]fs.DirEntry, error) {
	var out []fs.DirEntry
	for {
		count := n
		if n > 0 {
			count = n - len(out)
		}
		entries, err := d.ReadDirFile.ReadDir(count)
		for _, e := range entries {
			if e.Name() != ".s3site" {
				out = append(out, e)
			}
		}
		if err != nil || n <= 0 || len(out) == n {
			return out, err
		}
	}
}
