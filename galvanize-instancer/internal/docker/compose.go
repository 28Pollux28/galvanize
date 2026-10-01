package docker

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var transformer = transform.Chain(
	norm.NFD,
	runes.Remove(runes.In(unicode.Mn)), // Mn = non-spacing marks (the accent part)
	norm.NFC,
)
var invalidChars = regexp.MustCompile(`[^a-z0-9_-]+`)

func toASCII(s string) string {
	result, _, _ := transform.String(transformer, s)
	return result
}

func SanitizeProjectName(name string) string {
	s := toASCII(strings.ToLower(name))
	s = invalidChars.ReplaceAllString(s, "-")
	s = strings.TrimLeftFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	s = strings.TrimRight(s, "-_")
	return s
}

// projectSuffixLen is the number of hex characters of the identity hash
// appended to project names.
const projectSuffixLen = 8

// maxProjectNameLen keeps a project name usable as a single DNS label: it is
// also the instance's subdomain (<project>.<instancer_host>).
const maxProjectNameLen = 63

// BuildComposeProject returns the Docker Compose project name of a challenge
// instance, which also names its Traefik router and subdomain. It is made of
// a readable part (category, challenge name and team), truncated so the whole
// name fits a 63-character DNS label, and the first 8 hex characters of a
// SHA-1 of the instance's identity. The hash keeps apart instances whose
// readable parts are equal: same-named challenges in different categories,
// names that only differ in characters sanitization removes, or names cut by
// the truncation.
func BuildComposeProject(unique bool, category, challengeName, teamID string) string {
	var readable, identity string
	if unique {
		readable = "global-" + category + "-" + challengeName
		identity = "unique\x00" + category + "\x00" + challengeName
	} else {
		readable = "polypwn-" + category + "-" + challengeName + "-" + teamID
		identity = "team\x00" + category + "\x00" + challengeName + "\x00" + teamID
	}

	sum := sha1.Sum([]byte(identity))
	suffix := hex.EncodeToString(sum[:])[:projectSuffixLen]

	readable = SanitizeProjectName(readable)
	if maxReadable := maxProjectNameLen - 1 - projectSuffixLen; len(readable) > maxReadable {
		readable = strings.TrimRight(readable[:maxReadable], "-_")
	}
	return readable + "-" + suffix
}

// LegacyComposeProject returns the project name Galvanize v0.7.1 and earlier
// gave an instance. It ignored the category, and its suffix was constant:
// sha1.New().Sum(b) appends the hash of nothing to b instead of hashing b,
// so the first 6 hex characters were always those of "pol". It is kept only
// so instances started before an upgrade can still be terminated.
func LegacyComposeProject(unique bool, challengeName, teamID string) string {
	if unique {
		return SanitizeProjectName("global-" + challengeName)
	}
	composeProject := SanitizeProjectName("polypwn-" + challengeName + "-" + teamID)
	sum := sha1.New().Sum([]byte(composeProject))
	return composeProject + "-" + hex.EncodeToString(sum)[:6]
}
