package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRepositorySource_Allows(t *testing.T) {
	org := RepositorySource{URLPrefix: "https://github.com/org/"}
	for _, ok := range []string{
		"https://github.com/org/repo",
		"https://github.com/org/repo.git",
		"https://GitHub.com/org/repo.git",
		"https://github.com:443/org/repo.git",
		"https://github.com/org/team/repo.git",
	} {
		assert.True(t, org.Allows(ok), ok)
	}
	for _, bad := range []string{
		"https://github.com/org",                      // the folder itself
		"https://github.com/org/",                     // the folder itself
		"https://github.com/org-evil/repo",            // longer name, same start
		"https://github.com.evil.tld/org/repo",        // longer host, same start
		"https://github.com:8443/org/repo",            // other port
		"http://github.com/org/repo",                  // other scheme
		"https://github.com/org/../attacker/repo",     // dot segments
		"https://github.com/org/./repo",               // dot segments
		"https://github.com/org/%2e%2e/attacker/repo", // encoded dot segments
		"https://github.com/org//repo",                // empty segment
		"https://user:pass@github.com/org/repo",       // credentials
		"https://github.com/org/repo?x=1",             // query
		"https://github.com/org/repo#frag",            // fragment
		"https://github.com/org/re po",                // space
		"https://github.com/org\\repo",                // backslash
		"ext::sh -c touch% /tmp/pwned",                // command transport
		"git@github.com:org/repo.git",                 // scp syntax: not a URL
		"",
	} {
		assert.False(t, org.Allows(bad), bad)
	}

	// A prefix without a trailing slash allows nothing (Validate refuses it)
	assert.False(t, RepositorySource{URLPrefix: "https://github.com/org"}.Allows("https://github.com/org/repo"))

	host := RepositorySource{URLPrefix: "https://git.example.com/"}
	assert.True(t, host.Allows("https://git.example.com/any/repo.git"))
	assert.False(t, host.Allows("https://git.example.com.evil.tld/any/repo.git"))

	local := RepositorySource{URLPrefix: "file://"}
	assert.True(t, local.Allows("file:///srv/git/chals.git"))
	assert.False(t, local.Allows("file://host/srv/git/chals.git"))
}

func TestRepositorySource_ValidatePrefix(t *testing.T) {
	assert.NoError(t, RepositorySource{URLPrefix: "https://github.com/org/", Token: "t"}.ValidatePrefix())
	assert.NoError(t, RepositorySource{URLPrefix: "git://192.168.1.2/"}.ValidatePrefix())
	assert.NoError(t, RepositorySource{URLPrefix: "file://"}.ValidatePrefix())
	assert.Error(t, RepositorySource{URLPrefix: "https://github.com/org"}.ValidatePrefix(), "no trailing slash")
	assert.Error(t, RepositorySource{URLPrefix: "github.com/org/"}.ValidatePrefix(), "no scheme")
	assert.Error(t, RepositorySource{URLPrefix: "http://git.local/", Token: "t"}.ValidatePrefix(), "token over http")
	assert.Error(t, RepositorySource{URLPrefix: "https://u:p@github.com/org/"}.ValidatePrefix())

	ic := InstancerConfig{Repositories: RepositoriesConfig{Sources: []RepositorySource{{URLPrefix: "https://github.com/org"}}}}
	assert.ErrorContains(t, ic.Validate(), "repositories.sources[0]")
}
