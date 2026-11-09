package ansible

import (
	"strings"
	"testing"

	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/config"
	yaml "github.com/oasdiff/yaml3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newComposeChallenge(t *testing.T, composeDef string, expose []interface{}) *challenge.Challenge {
	t.Helper()
	params := map[string]interface{}{
		"compose_definition": composeDef,
	}
	if expose != nil {
		params["expose"] = expose
	}
	return &challenge.Challenge{
		Name:             "multi",
		Category:         "web",
		PlaybookName:     "custom_compose",
		Type:             "zync",
		DeployParameters: params,
	}
}

// parseResult re-parses the mutated compose_definition for assertions.
func parseResult(t *testing.T, params map[string]interface{}) map[string]interface{} {
	t.Helper()
	def, ok := params["compose_definition"].(string)
	require.True(t, ok, "compose_definition should be a string")
	var compose map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(def), &compose))
	return compose
}

func service(t *testing.T, compose map[string]interface{}, name string) map[string]interface{} {
	t.Helper()
	services, ok := compose["services"].(map[string]interface{})
	require.True(t, ok)
	svc, ok := services[name].(map[string]interface{})
	require.True(t, ok, "service %s should exist", name)
	return svc
}

func TestApplyComposeExposures_NoExposeIsNoOp(t *testing.T) {
	def := "services:\n  web:\n    image: nginx\n"
	chall := newComposeChallenge(t, def, nil)
	params := map[string]interface{}{"compose_definition": def}

	hints, err := applyComposeExposures(&config.Config{}, chall, "team1", params, nil)
	require.NoError(t, err)
	assert.Nil(t, hints)
	assert.Equal(t, def, params["compose_definition"], "definition should be untouched when no expose block")
}

func TestApplyComposeExposures_HTTPWiresTraefik(t *testing.T) {
	def := "services:\n  web:\n    image: nginx\n  db:\n    image: postgres\n"
	expose := []interface{}{
		map[string]interface{}{"service": "web", "port": 80, "type": "http"},
	}
	chall := newComposeChallenge(t, def, expose)
	params := map[string]interface{}{"compose_definition": def, "expose": expose}

	conf := &config.Config{}
	conf.Instancer.InstancerHost = "challs.example.com"
	conf.Instancer.ExtraDeploymentParameters = map[string]interface{}{"traefik_network": "traefik"}

	hints, err := applyComposeExposures(conf, chall, "team1", params, nil)
	require.NoError(t, err)
	assert.Empty(t, hints, "http exposures produce no protocol hints")

	compose := parseResult(t, params)
	web := service(t, compose, "web")

	labels, ok := web["labels"].(map[string]interface{})
	require.True(t, ok, "labels should be a map")
	assert.Equal(t, "true", labels["traefik.enable"])

	// Single http exposure → domain is <project>.<domain_root>.
	var ruleFound, portFound bool
	for k, v := range labels {
		if strings.HasPrefix(k, "traefik.http.routers.") && strings.HasSuffix(k, ".rule") {
			ruleFound = true
			assert.Contains(t, v, "challs.example.com")
		}
		if strings.HasPrefix(k, "traefik.http.services.") && strings.HasSuffix(k, ".loadbalancer.server.port") {
			portFound = true
			assert.Equal(t, "80", v)
		}
	}
	assert.True(t, ruleFound, "router rule label should be set")
	assert.True(t, portFound, "loadbalancer port label should be set")

	// Service stays on the implicit default network and also joins Traefik.
	nets, ok := web["networks"].([]interface{})
	require.True(t, ok)
	assert.Contains(t, nets, "default")
	assert.Contains(t, nets, "traefik")

	// Top-level external network declared.
	topNets, ok := compose["networks"].(map[string]interface{})
	require.True(t, ok)
	traefik, ok := topNets["traefik"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, traefik["external"])

	// expose helper key must not leak into Ansible vars.
	_, leaked := params["expose"]
	assert.False(t, leaked)
}

func TestApplyComposeExposures_HTTPRequiresTraefikNetwork(t *testing.T) {
	def := "services:\n  web:\n    image: nginx\n"
	expose := []interface{}{
		map[string]interface{}{"service": "web", "port": 80, "type": "http"},
	}
	chall := newComposeChallenge(t, def, expose)
	params := map[string]interface{}{"compose_definition": def, "expose": expose}

	conf := &config.Config{} // no traefik_network configured
	conf.Instancer.InstancerHost = "challs.example.com"

	_, err := applyComposeExposures(conf, chall, "team1", params, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "traefik_network")
}

func TestApplyComposeExposures_TCPPublishesFixedPort(t *testing.T) {
	def := "services:\n  ssh:\n    image: openssh\n"
	expose := []interface{}{
		map[string]interface{}{"service": "ssh", "port": 22, "type": "tcp", "scheme": "ssh"},
	}
	chall := newComposeChallenge(t, def, expose)
	params := map[string]interface{}{"compose_definition": def, "expose": expose}

	hints, err := applyComposeExposures(&config.Config{}, chall, "team1", params, nil)
	require.NoError(t, err)
	assert.Equal(t, "ssh", hints[22], "scheme hint should be propagated")

	compose := parseResult(t, params)
	ssh := service(t, compose, "ssh")
	ports, ok := ssh["ports"].([]interface{})
	require.True(t, ok)
	assert.Contains(t, ports, "22")
}

func TestApplyComposeExposures_TCPUsesRandomizedHostPort(t *testing.T) {
	def := "services:\n  ssh:\n    image: openssh\n"
	expose := []interface{}{
		map[string]interface{}{"service": "ssh", "port": 22, "type": "tcp"},
	}
	chall := newComposeChallenge(t, def, expose)
	params := map[string]interface{}{"compose_definition": def, "expose": expose}

	bindings := map[string]int{"22": 34567}
	hints, err := applyComposeExposures(&config.Config{}, chall, "team1", params, bindings)
	require.NoError(t, err)
	assert.Equal(t, "tcp", hints[22], "default scheme is tcp")

	compose := parseResult(t, params)
	ssh := service(t, compose, "ssh")
	ports, ok := ssh["ports"].([]interface{})
	require.True(t, ok)
	assert.Contains(t, ports, "34567:22")
}

func TestApplyComposeExposures_MultipleHTTPUsesSubdomains(t *testing.T) {
	def := "services:\n  web:\n    image: nginx\n  admin:\n    image: nginx\n"
	expose := []interface{}{
		map[string]interface{}{"service": "web", "port": 80, "type": "http"},
		map[string]interface{}{"service": "admin", "port": 8080, "type": "http"},
	}
	chall := newComposeChallenge(t, def, expose)
	params := map[string]interface{}{"compose_definition": def, "expose": expose}

	conf := &config.Config{}
	conf.Instancer.InstancerHost = "challs.example.com"
	conf.Instancer.ExtraDeploymentParameters = map[string]interface{}{"traefik_network": "traefik"}

	_, err := applyComposeExposures(conf, chall, "team1", params, nil)
	require.NoError(t, err)

	compose := parseResult(t, params)
	web := service(t, compose, "web")
	admin := service(t, compose, "admin")

	assert.True(t, hasRuleContaining(web, "web-"), "web service should get a web- subdomain rule")
	assert.True(t, hasRuleContaining(admin, "admin-"), "admin service should get an admin- subdomain rule")
}

func TestApplyComposeExposures_UnknownServiceErrors(t *testing.T) {
	def := "services:\n  web:\n    image: nginx\n"
	expose := []interface{}{
		map[string]interface{}{"service": "nope", "port": 80, "type": "http"},
	}
	chall := newComposeChallenge(t, def, expose)
	params := map[string]interface{}{"compose_definition": def, "expose": expose}

	conf := &config.Config{}
	conf.Instancer.ExtraDeploymentParameters = map[string]interface{}{"traefik_network": "traefik"}

	_, err := applyComposeExposures(conf, chall, "team1", params, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
}

func TestExposeTCPContainerPorts(t *testing.T) {
	params := map[string]interface{}{
		"expose": []interface{}{
			map[string]interface{}{"service": "ssh", "port": 22, "type": "tcp"},
			map[string]interface{}{"service": "web", "port": 80, "type": "http"},
			map[string]interface{}{"service": "dns", "port": 53, "type": "tcp"},
		},
	}
	ports := exposeTCPContainerPorts(params)
	assert.ElementsMatch(t, []string{"22", "53"}, ports)
}

func hasRuleContaining(svc map[string]interface{}, substr string) bool {
	labels, ok := svc["labels"].(map[string]interface{})
	if !ok {
		return false
	}
	for k := range labels {
		if strings.HasPrefix(k, "traefik.http.routers.") && strings.Contains(k, substr) {
			return true
		}
	}
	return false
}
