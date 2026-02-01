package ansible

import (
	"encoding/json"
	"fmt"
	"io"

	"go.uber.org/zap"
)

// ContainerInfo defines the structure for a single item in the 'containers' list.
type ContainerInfo struct {
	Command    string            `json:"Command"`
	CreatedAt  string            `json:"CreatedAt"`
	ID         string            `json:"ID"`
	Image      string            `json:"Image"`
	Name       string            `json:"Name"`
	Ports      string            `json:"Ports"`
	State      string            `json:"State"`
	Status     string            `json:"Status"`
	Labels     map[string]string `json:"Labels"`
	Networks   []string          `json:"Networks"`
	Publishers []PublisherInfo   `json:"Publishers"`
}

// PublisherInfo is for the array inside the ContainerInfo
type PublisherInfo struct {
	Protocol      string `json:"Protocol"`
	PublishedPort int    `json:"PublishedPort"`
	TargetPort    int    `json:"TargetPort"`
	URL           string `json:"URL"`
}

// Minimal structs to navigate the JSON path to the 'containers' field.
// We use json.RawMessage to capture the raw JSON for the nested 'containers' list.
type TaskHosts struct {
	Action     json.RawMessage `json:"action"`
	Containers json.RawMessage `json:"containers"`
}

type TaskResult struct {
	Hosts map[string]TaskHosts `json:"hosts"` // Key is the IP address
}

type PlayTasks struct {
	Tasks []TaskResult `json:"tasks"`
}

type RawRoot struct {
	Plays []PlayTasks `json:"plays"`
}

// ExtractContainerInfo reads JSON data from a reader and extracts the
// container details, bypassing incomplete external library structs.
func ExtractContainerInfo(r io.Reader) ([]ContainerInfo, error) {
	// Read all data from the reader into a byte slice
	jsonBytes, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read from reader: %w", err)
	}
	// Unmarshal into the RawRoot struct to access the RawMessage
	var rawRoot RawRoot
	if err := json.Unmarshal(jsonBytes, &rawRoot); err != nil {
		zap.S().Debugf("Failed to unmarshal JSON into RawRoot: %s", jsonBytes)
		return nil, fmt.Errorf("failed to unmarshal JSON into raw root structure: %w", err)
	}

	var allContainers []ContainerInfo

	// Navigate the structure to find the 'containers' RawMessage
	if len(rawRoot.Plays) == 0 {
		return nil, fmt.Errorf("JSON structure missing 'plays' array")
	}

	// Assuming the container information is in the first play's first task.
	// You may need to iterate more if you expect multiple plays/tasks.
	if len(rawRoot.Plays[0].Tasks) == 0 {
		return nil, fmt.Errorf("JSON structure missing tasks in the first play")
	}

	for _, taskResult := range rawRoot.Plays[0].Tasks {
		// Iterate through all host results within the task
		for _, hostResult := range taskResult.Hosts {
			var action string
			if err := json.Unmarshal(hostResult.Action, &action); err != nil {
				return nil, fmt.Errorf("failed to unmarshal task action: %w", err)
			}
			if action != "community.docker.docker_compose_v2" {
				zap.S().Debugf("Skipping action %s for host, not matching expected docker_compose_v2", action)
				continue
			}
			// Unmarshal the raw bytes (RawMessage) into the final ContainerInfo struct
			var containersForHost []ContainerInfo
			if len(hostResult.Containers) > 0 {
				if err := json.Unmarshal(hostResult.Containers, &containersForHost); err != nil {
					return nil, fmt.Errorf("failed to unmarshal container details for a host: %w", err)
				}
				allContainers = append(allContainers, containersForHost...)
			}
		}
	}

	return allContainers, nil
}
