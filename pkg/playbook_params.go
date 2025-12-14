package pkg

import "fmt"

var playbookParams = map[string][]string{
	"tcp": {
		"image",
		"published_ports",
	},
	"static_http":    {},
	"dynamic_http":   {},
	"custom_compose": {},
}

func validatePlaybookParams(playbook string, params map[string]interface{}) error {
	for _, param := range playbookParams[playbook] {
		if _, ok := params[param]; !ok {
			return fmt.Errorf("missing required parameter: %s", param)
		}
	}
	return nil
}
