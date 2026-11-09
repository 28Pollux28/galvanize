package ansible

import (
	"fmt"
	"sort"
	"strings"
)

// GetConnectionInfo returns the first Traefik HTTP URL found, or else the
// published ports. When showTCPPorts is set, the published ports of every
// container are listed after the HTTP URL instead of being hidden by it.
func GetConnectionInfo(containerInfos []ContainerInfo, host string, protocolHints map[int]string, showTCPPorts bool) (string, error) {
	if showTCPPorts {
		return getAllConnectionInfo(containerInfos, host, protocolHints)
	}
	traefikLabel := ""
	for _, ci := range containerInfos {
		for labelKey, labelValue := range ci.Labels {
			if strings.HasPrefix(labelKey, "traefik.http.routers.") {
				traefikLabel = labelValue
				break
			}
		}
		// Check if we have a traefik label
		if traefikLabel != "" {
			labelParts := strings.Split(traefikLabel, "`")
			if len(labelParts) >= 2 {
				domainName := "https://" + labelParts[1] + "/"
				return domainName, nil
			}
		}
		// Check if we have published ports
		ports := publishedPortURLs(ci.Publishers, host, protocolHints)
		if len(ports) > 0 {
			return strings.Join(ports, "\n"), nil
		}
	}
	return "", fmt.Errorf("no connection info found")
}

func getAllConnectionInfo(containerInfos []ContainerInfo, host string, protocolHints map[int]string) (string, error) {
	var lines []string
	for _, ci := range containerInfos {
		if url := traefikURL(ci.Labels); url != "" {
			lines = append(lines, url)
			break
		}
	}
	var publishers []PublisherInfo
	for _, ci := range containerInfos {
		publishers = append(publishers, ci.Publishers...)
	}
	// Docker does not guarantee publisher order; sort so the output is stable.
	sort.SliceStable(publishers, func(i, j int) bool {
		if publishers[i].TargetPort != publishers[j].TargetPort {
			return publishers[i].TargetPort < publishers[j].TargetPort
		}
		return publishers[i].PublishedPort < publishers[j].PublishedPort
	})
	seen := map[string]bool{}
	for _, port := range publishedPortURLs(publishers, host, protocolHints) {
		if !seen[port] {
			seen[port] = true
			lines = append(lines, port)
		}
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("no connection info found")
	}
	return strings.Join(lines, "\n"), nil
}

func traefikURL(labels map[string]string) string {
	for labelKey, labelValue := range labels {
		if !strings.HasPrefix(labelKey, "traefik.http.routers.") {
			continue
		}
		labelParts := strings.Split(labelValue, "`")
		if len(labelParts) >= 2 {
			return "https://" + labelParts[1] + "/"
		}
	}
	return ""
}

func publishedPortURLs(publishers []PublisherInfo, host string, protocolHints map[int]string) []string {
	var ports []string
	for _, pub := range publishers {
		if pub.PublishedPort != 0 {
			// If IP is IPv6, continue
			if strings.Contains(pub.URL, ":") {
				continue
			}
			scheme := pub.Protocol
			if protocolHints != nil {
				if hintedScheme, ok := protocolHints[pub.TargetPort]; ok {
					scheme = hintedScheme
				}
			}
			ports = append(ports, fmt.Sprintf("%s://%s:%d", scheme, host, pub.PublishedPort))
		}
	}
	return ports
}
