package ansible

import (
	"fmt"
	"strconv"

	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/internal/docker"
	"github.com/28Pollux28/galvanize/pkg/config"
	yaml "github.com/oasdiff/yaml3"
)

// traefikNetworkParam is the extra_deployment_parameters key that names the
// external Docker network Traefik watches. It is the same value the http/tcp
// playbooks consume.
const traefikNetworkParam = "traefik_network"

// exposeTCPContainerPorts returns the container ports (as strings) of every tcp
// exposure, so the deployer can reserve persistent random host ports for them
// using the same machinery as the tcp playbook's published_ports.
func exposeTCPContainerPorts(params map[string]interface{}) []string {
	exposures, err := challenge.ParseExposures(params)
	if err != nil || len(exposures) == 0 {
		return nil
	}
	var ports []string
	for _, exp := range exposures {
		if exp.Type == challenge.ExposeTCP {
			ports = append(ports, strconv.Itoa(exp.Port))
		}
	}
	return ports
}

// applyComposeExposures wires up networking for compose challenges that declare
// a deploy_parameters.expose block, so they get the same automatic Traefik
// (http) and published-port (tcp) handling the single-container playbooks
// provide.
//
// It mutates params["compose_definition"] in place and returns protocol hints
// (keyed by container/target port) used when rendering connection info.
//
// portBindings maps a container port (as a string, e.g. "22") to a persisted
// random host port; it is empty when port randomization is disabled, in which
// case tcp exposures publish the container port directly.
func applyComposeExposures(conf *config.Config, chall *challenge.Challenge, teamID string, params map[string]interface{}, portBindings map[string]int) (map[int]string, error) {
	exposures, err := challenge.ParseExposures(chall.DeployParameters)
	if err != nil {
		return nil, err
	}
	if len(exposures) == 0 {
		return nil, nil
	}

	defStr, ok := params["compose_definition"].(string)
	if !ok || defStr == "" {
		return nil, fmt.Errorf("expose requires a compose_definition")
	}

	var compose map[string]interface{}
	if err := yaml.Unmarshal([]byte(defStr), &compose); err != nil {
		return nil, fmt.Errorf("failed to parse compose definition for exposure wiring: %w", err)
	}
	services, ok := compose["services"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("compose definition has no services map")
	}

	project := docker.BuildComposeProject(chall.Unique, chall.Category, chall.Name, teamID)
	domainRoot := conf.Instancer.InstancerHost
	traefikNetwork, _ := conf.Instancer.ExtraDeploymentParameters[traefikNetworkParam].(string)

	httpCount := 0
	for _, exp := range exposures {
		if exp.Type == challenge.ExposeHTTP {
			httpCount++
		}
	}

	hints := map[int]string{}
	needTraefikNetwork := false

	for _, exp := range exposures {
		svc, ok := services[exp.Service].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("expose references unknown or invalid service %q", exp.Service)
		}

		switch exp.Type {
		case challenge.ExposeHTTP:
			if traefikNetwork == "" {
				return nil, fmt.Errorf("http exposure for service %q requires %q in extra_deployment_parameters", exp.Service, traefikNetworkParam)
			}
			routerName := project
			domain := project + "." + domainRoot
			if httpCount > 1 {
				routerName = docker.SanitizeProjectName(exp.Service+"-"+project)
				domain = docker.SanitizeProjectName(exp.Service+"-"+project) + "." + domainRoot
			}
			applyTraefikLabels(svc, routerName, domain, exp.Port)
			attachNetwork(svc, traefikNetwork)
			needTraefikNetwork = true
		case challenge.ExposeTCP:
			portSpec := strconv.Itoa(exp.Port)
			if hostPort, ok := portBindings[strconv.Itoa(exp.Port)]; ok && hostPort > 0 {
				portSpec = strconv.Itoa(hostPort) + ":" + strconv.Itoa(exp.Port)
			}
			addPort(svc, portSpec)
			scheme := exp.Scheme
			if scheme == "" {
				scheme = "tcp"
			}
			hints[exp.Port] = scheme
		}

		services[exp.Service] = svc
	}

	if needTraefikNetwork {
		ensureExternalNetwork(compose, traefikNetwork)
	}

	out, err := yaml.Marshal(compose)
	if err != nil {
		return nil, fmt.Errorf("failed to re-serialize compose definition: %w", err)
	}
	params["compose_definition"] = string(out)
	delete(params, "expose")

	return hints, nil
}

// applyTraefikLabels adds the Traefik router/service labels to a compose
// service, mirroring the http playbook. Existing labels are preserved.
func applyTraefikLabels(svc map[string]interface{}, routerName, domain string, port int) {
	labels := normalizeLabels(svc["labels"])
	labels["traefik.enable"] = "true"
	labels["traefik.http.routers."+routerName+".rule"] = fmt.Sprintf("Host(`%s`)", domain)
	labels["traefik.http.services."+routerName+".loadbalancer.server.port"] = strconv.Itoa(port)
	svc["labels"] = labels
}

// normalizeLabels converts compose labels (list of "k=v" strings or a map) into
// a string-keyed map so new labels can be merged in.
func normalizeLabels(existing interface{}) map[string]interface{} {
	labels := map[string]interface{}{}
	switch v := existing.(type) {
	case map[string]interface{}:
		for k, val := range v {
			labels[k] = val
		}
	case []interface{}:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				continue
			}
			for i := 0; i < len(s); i++ {
				if s[i] == '=' {
					labels[s[:i]] = s[i+1:]
					break
				}
			}
		}
	}
	return labels
}

// attachNetwork ensures a service is connected to the named network, preserving
// any existing network configuration.
func attachNetwork(svc map[string]interface{}, network string) {
	switch v := svc["networks"].(type) {
	case nil:
		svc["networks"] = []interface{}{network}
	case []interface{}:
		for _, n := range v {
			if s, ok := n.(string); ok && s == network {
				return
			}
		}
		svc["networks"] = append(v, network)
	case map[string]interface{}:
		if _, ok := v[network]; !ok {
			v[network] = map[string]interface{}{}
		}
		svc["networks"] = v
	}
}

// addPort appends a port specification to a service's ports list.
func addPort(svc map[string]interface{}, portSpec string) {
	switch v := svc["ports"].(type) {
	case nil:
		svc["ports"] = []interface{}{portSpec}
	case []interface{}:
		for _, p := range v {
			if s, ok := p.(string); ok && s == portSpec {
				return
			}
		}
		svc["ports"] = append(v, portSpec)
	default:
		svc["ports"] = []interface{}{portSpec}
	}
}

// ensureExternalNetwork declares the given network as external at the top level
// of the compose definition so Traefik can reach the service.
func ensureExternalNetwork(compose map[string]interface{}, network string) {
	networks, ok := compose["networks"].(map[string]interface{})
	if !ok {
		networks = map[string]interface{}{}
	}
	if _, exists := networks[network]; !exists {
		networks[network] = map[string]interface{}{"external": true}
	}
	compose["networks"] = networks
}
