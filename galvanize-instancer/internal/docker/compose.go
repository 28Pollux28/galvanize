package docker

import (
	"crypto/sha1"
	"encoding/hex"
)

func BuildComposeProject(params map[string]interface{}, challengeName, teamID string) string {
	var composeProject string
	if unique, ok := params["unique"].(bool); ok && unique == true {
		composeProject = "global-" + challengeName
	} else {
		composeProject = "polypwn-" + challengeName + "-" + teamID
		sum := sha1.New().Sum([]byte(composeProject))
		hexSum := hex.EncodeToString(sum)
		composeProject = composeProject + "-" + hexSum[:6]
	}
	return composeProject
}
