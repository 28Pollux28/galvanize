package ansible

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// hostRule matches each Host(`name`) of a Traefik router rule
var hostRule = regexp.MustCompile("Host\\(`([^`]+)`\\)")

// GetConnectionInfo returns how players reach a deployed instance, one
// endpoint per line. Every container routed by Traefik gives https://<host>/
// for each Host() of its routers' rules, containers taken in compose service
// order so the result does not depend on the order Ansible reports them in.
// Published ports give <scheme>://<host>:<port>, the scheme being the port's
// protocol hint, or else the port's protocol (tcp, udp), sorted by container
// port. They are listed when no container is routed by Traefik, or after the
// HTTP URLs when showTCPPorts is set.
func GetConnectionInfo(containerInfos []ContainerInfo, host string, protocolHints map[int]string, showTCPPorts bool) (string, error) {
	containers := append([]ContainerInfo(nil), containerInfos...)
	sort.SliceStable(containers, func(i, j int) bool {
		return containerSortKey(containers[i]) < containerSortKey(containers[j])
	})

	var endpoints []string
	seen := map[string]bool{}
	add := func(endpoint string) {
		if !seen[endpoint] {
			seen[endpoint] = true
			endpoints = append(endpoints, endpoint)
		}
	}

	for _, ci := range containers {
		for _, domain := range traefikDomains(ci.Labels) {
			add("https://" + domain + "/")
		}
	}

	if len(endpoints) == 0 || showTCPPorts {
		var publishers []PublisherInfo
		for _, ci := range containers {
			publishers = append(publishers, ci.Publishers...)
		}
		// Docker does not guarantee publisher order; sort so the output is stable
		sort.SliceStable(publishers, func(i, j int) bool {
			if publishers[i].TargetPort != publishers[j].TargetPort {
				return publishers[i].TargetPort < publishers[j].TargetPort
			}
			return publishers[i].PublishedPort < publishers[j].PublishedPort
		})
		for _, pub := range publishers {
			// Docker lists each port for IPv4 and IPv6: keep the IPv4 one
			if pub.PublishedPort == 0 || strings.Contains(pub.URL, ":") {
				continue
			}
			scheme := pub.Protocol
			if hinted, ok := protocolHints[pub.TargetPort]; ok {
				scheme = hinted
			}
			add(fmt.Sprintf("%s://%s:%d", scheme, host, pub.PublishedPort))
		}
	}

	if len(endpoints) == 0 {
		return "", fmt.Errorf("no connection info found")
	}
	return strings.Join(endpoints, "\n"), nil
}

// traefikDomains returns the hosts of the container's Traefik HTTP router
// rules, sorted by router. Other router labels (entrypoints, tls,
// middlewares...) are not rules and are ignored, as are containers Traefik is
// told to leave alone.
func traefikDomains(labels map[string]string) []string {
	if strings.EqualFold(labels["traefik.enable"], "false") {
		return nil
	}
	var ruleKeys []string
	for key := range labels {
		if strings.HasPrefix(key, "traefik.http.routers.") && strings.HasSuffix(key, ".rule") {
			ruleKeys = append(ruleKeys, key)
		}
	}
	sort.Strings(ruleKeys)

	var domains []string
	for _, key := range ruleKeys {
		for _, m := range hostRule.FindAllStringSubmatch(labels[key], -1) {
			domains = append(domains, m[1])
		}
	}
	return domains
}

// containerSortKey orders containers by compose service, then by name
func containerSortKey(ci ContainerInfo) string {
	return ci.Labels["com.docker.compose.service"] + "\x00" + ci.Name
}
