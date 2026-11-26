package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Repository URLs are compared structurally, never as strings: a plain
// prefix test lets "https://github.com/org" allow "https://github.com/org-evil/x",
// "https://git.example.com" allow "https://git.example.com.evil.tld/x" (which
// then receives the token), and "https://github.com/org/../other/x" escape the
// organisation.

// repoSchemes are the URL schemes repositories may be cloned with.
var repoSchemes = map[string]string{"https": "443", "http": "80", "ssh": "22", "git": "9418", "file": ""}

// repoURL is a parsed repository URL or source prefix.
type repoURL struct {
	scheme, host, port string
	// segments of the path, without the empty first one; a trailing slash
	// gives an empty last segment
	segments []string
}

// parseRepoURL parses and checks a repository URL. Percent-encoding,
// backslashes, credentials, queries, fragments and "." or ".." segments are
// refused: they are never needed and each one is a way to make two
// comparisons of the same URL disagree.
func parseRepoURL(raw string) (*repoURL, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty URL")
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f || r == '\\' || r == '%' {
			return nil, fmt.Errorf("%q: spaces, control characters, backslashes and percent-encoding are not allowed", raw)
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	defaultPort, ok := repoSchemes[scheme]
	if !ok {
		return nil, fmt.Errorf("%q: the scheme must be https, http, ssh, git or file", raw)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%q: credentials in the URL are not allowed; configure a token on the source instead", raw)
	}
	if u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, fmt.Errorf("%q: queries and fragments are not allowed", raw)
	}
	host := strings.ToLower(u.Hostname())
	if scheme != "file" && host == "" {
		return nil, fmt.Errorf("%q has no host", raw)
	}
	if scheme == "file" && host != "" {
		return nil, fmt.Errorf("%q: file URLs take no host (file:///path)", raw)
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("%q: invalid path", raw)
	}
	segments := strings.Split(path[1:], "/")
	for i, s := range segments {
		if s == "." || s == ".." {
			return nil, fmt.Errorf("%q: \".\" and \"..\" path segments are not allowed", raw)
		}
		if s == "" && i != len(segments)-1 {
			return nil, fmt.Errorf("%q: empty path segment", raw)
		}
	}
	return &repoURL{scheme: scheme, host: host, port: port, segments: segments}, nil
}

// ValidatePrefix checks url_prefix: a URL whose path ends with "/", so that
// it names a host or a folder of it, never the start of a name.
func (s RepositorySource) ValidatePrefix() error {
	p, err := parseRepoURL(s.URLPrefix)
	if err != nil {
		return err
	}
	if p.segments[len(p.segments)-1] != "" {
		return fmt.Errorf("%q must end with \"/\" (e.g. https://github.com/your-org/)", s.URLPrefix)
	}
	if s.Token != "" && p.scheme != "https" {
		return fmt.Errorf("%q: a token can only be sent over https", s.URLPrefix)
	}
	return nil
}

// Allows reports whether rawURL is a repository below the source: same
// scheme, host and port, and a path continuing url_prefix's folders.
func (s RepositorySource) Allows(rawURL string) bool {
	p, err := parseRepoURL(s.URLPrefix)
	if err != nil || p.segments[len(p.segments)-1] != "" {
		return false
	}
	u, err := parseRepoURL(rawURL)
	if err != nil {
		return false
	}
	if u.scheme != p.scheme || u.host != p.host || u.port != p.port {
		return false
	}
	folders := p.segments[:len(p.segments)-1]
	if len(u.segments) <= len(folders) || u.segments[len(folders)] == "" {
		return false // the URL must name something inside the folder
	}
	for i, f := range folders {
		if u.segments[i] != f {
			return false
		}
	}
	return true
}

// CheckRepositoryURL returns an error explaining why rawURL cannot be used.
func CheckRepositoryURL(rawURL string) error {
	_, err := parseRepoURL(rawURL)
	return err
}

// SourceFor returns the source allowing rawURL, or nil.
func (r RepositoriesConfig) SourceFor(rawURL string) *RepositorySource {
	for i := range r.Sources {
		if r.Sources[i].Allows(rawURL) {
			return &r.Sources[i]
		}
	}
	return nil
}
