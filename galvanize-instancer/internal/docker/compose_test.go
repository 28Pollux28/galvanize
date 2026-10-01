package docker

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A Docker Compose project name that is also a valid DNS label
var validProject = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func TestBuildComposeProject_SameNameDifferentCategories(t *testing.T) {
	web := BuildComposeProject(false, "web", "login", "team1")
	pwn := BuildComposeProject(false, "pwn", "login", "team1")

	assert.NotEqual(t, web, pwn)
	assert.True(t, strings.HasPrefix(web, "polypwn-web-login-team1-"), web)
	assert.True(t, strings.HasPrefix(pwn, "polypwn-pwn-login-team1-"), pwn)

	assert.NotEqual(t,
		BuildComposeProject(true, "web", "login", ""),
		BuildComposeProject(true, "pwn", "login", ""))
}

func TestBuildComposeProject_Format(t *testing.T) {
	// The example in data/challenges/example/http/challenge.yml
	assert.Equal(t, "polypwn-web-example-http-team1-9448f1e7", BuildComposeProject(false, "web", "example-http", "team1"))
}

func TestBuildComposeProject_SuffixDependsOnIdentity(t *testing.T) {
	a := BuildComposeProject(false, "web", "login", "team1")
	assert.Equal(t, a, BuildComposeProject(false, "web", "login", "team1"), "deterministic")
	assert.NotEqual(t, a, BuildComposeProject(false, "web", "login", "team2"))

	// Readable parts equal after sanitization: only the hash keeps them apart
	assert.NotEqual(t,
		BuildComposeProject(false, "web", "Login!", "team1"),
		BuildComposeProject(false, "web", "login?", "team1"))

	// The v0.7.1 suffix was always "706f6c"
	assert.False(t, strings.HasSuffix(a, "-706f6c"), a)
}

func TestBuildComposeProject_UniqueAndTeamInstancesDiffer(t *testing.T) {
	assert.NotEqual(t,
		BuildComposeProject(true, "web", "login", ""),
		BuildComposeProject(false, "web", "login", ""))
	assert.True(t, strings.HasPrefix(BuildComposeProject(true, "web", "login", ""), "global-web-login-"))
}

func TestBuildComposeProject_FitsOneDNSLabel(t *testing.T) {
	long := strings.Repeat("very-long-challenge-name-", 6)
	a := BuildComposeProject(false, "reverse-engineering", long+"a", "team-with-a-long-identifier")
	b := BuildComposeProject(false, "reverse-engineering", long+"b", "team-with-a-long-identifier")

	for _, p := range []string{a, b} {
		assert.LessOrEqual(t, len(p), 63, p)
		assert.Regexp(t, validProject, p)
	}
	// Truncated to the same readable part, still distinct
	assert.NotEqual(t, a, b)
}

func TestBuildComposeProject_ValidForAnyName(t *testing.T) {
	for _, p := range []string{
		BuildComposeProject(false, "Wéb", "Ünïcode Chall", "Team 1"),
		BuildComposeProject(false, "", "", ""),
		BuildComposeProject(true, "!!!", "???", ""),
	} {
		assert.Regexp(t, validProject, p)
	}
	// Nothing readable left after sanitization but the prefix: still unique
	assert.Regexp(t, `^global-[0-9a-f]{8}$`, BuildComposeProject(true, "中文", "挑战", ""))
	assert.NotEqual(t, BuildComposeProject(true, "中文", "挑战", ""), BuildComposeProject(true, "日本", "語", ""))
}

func TestLegacyComposeProject_MatchesV071(t *testing.T) {
	// What Galvanize v0.7.1 named these instances
	assert.Equal(t, "polypwn-login-team1-706f6c", LegacyComposeProject(false, "login", "team1"))
	assert.Equal(t, "polypwn-my-chall-team-2-706f6c", LegacyComposeProject(false, "My Chall", "Team 2"))
	assert.Equal(t, "global-login", LegacyComposeProject(true, "login", ""))
}

func TestLegacyComposeProject_DiffersFromNewName(t *testing.T) {
	assert.NotEqual(t, LegacyComposeProject(false, "login", "team1"), BuildComposeProject(false, "web", "login", "team1"))
	assert.NotEqual(t, LegacyComposeProject(true, "login", ""), BuildComposeProject(true, "web", "login", ""))
}
