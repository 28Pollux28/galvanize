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

func BuildComposeProject(unique bool, challengeName, teamID string) string {
	var composeProject string
	if unique == true {
		composeProject = "global-" + challengeName
		composeProject = SanitizeProjectName(composeProject)
	} else {
		composeProject = "polypwn-" + challengeName + "-" + teamID
		composeProject := SanitizeProjectName(composeProject)
		sum := sha1.New().Sum([]byte(composeProject))
		hexSum := hex.EncodeToString(sum)
		composeProject = composeProject + "-" + hexSum[:6]
	}
	return composeProject
}
