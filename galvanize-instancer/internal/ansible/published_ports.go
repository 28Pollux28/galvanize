package ansible

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/28Pollux28/galvanize/pkg/config"
)

var knownDockerProtocols = map[string]struct{}{
	"tcp":  {},
	"udp":  {},
	"sctp": {},
}

func randomizableContainerPorts(params map[string]interface{}) []string {
	rawPorts, ok := params["published_ports"]
	if !ok {
		return nil
	}
	ports, ok := rawPorts.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		switch v := p.(type) {
		case int:
			out = append(out, strconv.Itoa(v))
		case int64:
			out = append(out, strconv.FormatInt(v, 10))
		case float64:
			out = append(out, strconv.Itoa(int(v)))
		case string:
			composePort, _, _ := splitPortHint(v)
			if !hasExplicitHostPortMapping(composePort) {
				out = append(out, composePort)
			}
		}
	}
	return out
}

// normalizePublishedPortsWithState normalizes published_ports and optionally
// randomizes non-fixed host-port bindings.
//
// existingBindings and resulting bindings use container port specs as keys,
// for example "22", "53/udp", "8080:80".
func normalizePublishedPortsWithState(params map[string]interface{}, randomizePorts bool, existingBindings map[string]int, createState bool, portRng portRange) (map[string]interface{}, map[int]string, map[string]int) {
	normalized := make(map[string]interface{}, len(params))
	for k, v := range params {
		normalized[k] = v
	}

	rawPorts, ok := params["published_ports"]
	if !ok {
		return normalized, map[int]string{}, map[string]int{}
	}

	ports, ok := rawPorts.([]interface{})
	if !ok {
		return normalized, map[int]string{}, map[string]int{}
	}

	normalizedPorts := make([]interface{}, 0, len(ports))
	hints := make(map[int]string)
	usedHostPorts := make(map[int]struct{})
	stateDirty := false
	deploymentBindings := make(map[string]int)
	for k, v := range existingBindings {
		deploymentBindings[k] = v
		if v > 0 {
			usedHostPorts[v] = struct{}{}
		}
	}

	for _, p := range ports {
		switch v := p.(type) {
		case int:
			if randomizePorts {
				normalizedPorts = append(normalizedPorts, withPersistentRandomBinding(strconv.Itoa(v), deploymentBindings, usedHostPorts, createState, &stateDirty, portRng))
			} else {
				normalizedPorts = append(normalizedPorts, v)
			}
		case int64:
			if randomizePorts {
				normalizedPorts = append(normalizedPorts, withPersistentRandomBinding(strconv.FormatInt(v, 10), deploymentBindings, usedHostPorts, createState, &stateDirty, portRng))
			} else {
				normalizedPorts = append(normalizedPorts, v)
			}
		case float64:
			intPort := int(v)
			if randomizePorts {
				normalizedPorts = append(normalizedPorts, withPersistentRandomBinding(strconv.Itoa(intPort), deploymentBindings, usedHostPorts, createState, &stateDirty, portRng))
			} else {
				normalizedPorts = append(normalizedPorts, intPort)
			}
		case string:
			composePort, targetPort, hint := splitPortHint(v)
			if randomizePorts && !hasExplicitHostPortMapping(composePort) {
				composePort = withPersistentRandomBinding(composePort, deploymentBindings, usedHostPorts, createState, &stateDirty, portRng)
			}
			normalizedPorts = append(normalizedPorts, composePort)
			if targetPort > 0 && hint != "" {
				hints[targetPort] = hint
			}
		default:
			// Keep unknown types untouched to avoid breaking existing challenge files.
			normalizedPorts = append(normalizedPorts, p)
		}
	}

	normalized["published_ports"] = normalizedPorts
	if randomizePorts && createState && stateDirty {
		return normalized, hints, deploymentBindings
	}
	if randomizePorts {
		return normalized, hints, deploymentBindings
	}
	return normalized, hints, map[string]int{}
}

func splitPortHint(raw string) (composePort string, targetPort int, hint string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw, 0, ""
	}

	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 {
		return trimmed, parseTargetPort(trimmed), ""
	}

	maybeHint := strings.ToLower(strings.TrimSpace(parts[len(parts)-1]))
	if maybeHint == "" {
		return trimmed, parseTargetPort(trimmed), ""
	}
	if _, isDockerProtocol := knownDockerProtocols[maybeHint]; isDockerProtocol {
		return trimmed, parseTargetPort(trimmed), ""
	}

	base := strings.Join(parts[:len(parts)-1], "/")
	base = strings.TrimSpace(base)
	if base == "" {
		return trimmed, 0, ""
	}

	return base, parseTargetPort(base), maybeHint
}

func parseTargetPort(portDef string) int {
	trimmed := strings.TrimSpace(portDef)
	if trimmed == "" {
		return 0
	}
	segments := strings.Split(trimmed, ":")
	last := strings.TrimSpace(segments[len(segments)-1])
	if last == "" || strings.Contains(last, "-") {
		return 0
	}
	value, err := strconv.Atoi(last)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func hasExplicitHostPortMapping(portDef string) bool {
	return strings.Contains(portDef, ":")
}

func randomPortBinding(containerPort string, usedHostPorts map[int]struct{}, portRng portRange) string {
	for range 128 {
		hostPort := portRng.random()
		if hostPort == 0 {
			continue
		}
		if _, used := usedHostPorts[hostPort]; used {
			continue
		}
		usedHostPorts[hostPort] = struct{}{}
		return strconv.Itoa(hostPort) + ":" + containerPort
	}

	// Extremely unlikely fallback if random generation keeps colliding.
	for port := portRng.lo; port <= portRng.hi; port++ {
		if _, used := usedHostPorts[port]; used {
			continue
		}
		usedHostPorts[port] = struct{}{}
		return strconv.Itoa(port) + ":" + containerPort
	}

	// No free port in range; return unchanged to avoid crashing deployment.
	return containerPort
}

func withPersistentRandomBinding(containerPort string, deploymentBindings map[string]int, usedHostPorts map[int]struct{}, createState bool, stateDirty *bool, portRng portRange) string {
	if deploymentBindings != nil {
		if existingHostPort, ok := deploymentBindings[containerPort]; ok {
			usedHostPorts[existingHostPort] = struct{}{}
			return strconv.Itoa(existingHostPort) + ":" + containerPort
		}
	}
	if !createState {
		return containerPort
	}
	binding := randomPortBinding(containerPort, usedHostPorts, portRng)
	parts := strings.Split(binding, ":")
	if len(parts) == 2 && deploymentBindings != nil {
		hostPort, err := strconv.Atoi(parts[0])
		if err == nil {
			deploymentBindings[containerPort] = hostPort
			*stateDirty = true
		}
	}
	return binding
}

// portRange is the range, bounds included, randomized host ports are
// picked from (instancer.randomized_port_min and randomized_port_max).
type portRange struct {
	lo, hi int
}

func randomizedPortRange(conf *config.Config) portRange {
	lo, hi := conf.Instancer.RandomizedPortRange()
	return portRange{lo: lo, hi: hi}
}

func (r portRange) contains(port int) bool {
	return port >= r.lo && port <= r.hi
}

func (r portRange) String() string {
	return fmt.Sprintf("%d-%d", r.lo, r.hi)
}

// random returns a port of the range picked at random, or 0 if the range is
// empty or the system's random source fails.
func (r portRange) random() int {
	if r.lo > r.hi {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(r.hi-r.lo+1)))
	if err != nil {
		return 0
	}
	return int(v.Int64()) + r.lo
}
