package ansible

import (
	"strconv"
	"strings"
	"testing"

	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizePublishedPorts_ExtractsOptionalProtocolHints(t *testing.T) {
	params := map[string]interface{}{
		"image": "example:latest",
		"published_ports": []interface{}{
			"22/ssh",
			"8080:80/http",
			"53/udp",
			443,
		},
	}

	normalized, hints, bindings := normalizePublishedPortsWithState(params, false, nil, false, defaultPortRange)

	assert.Equal(t, "example:latest", normalized["image"])
	assert.Empty(t, bindings)

	ports, ok := normalized["published_ports"].([]interface{})
	require.True(t, ok)
	assert.Equal(t, []interface{}{"22", "8080:80", "53/udp", 443}, ports)

	assert.Equal(t, "ssh", hints[22])
	assert.Equal(t, "http", hints[80])
	_, hasUDPHint := hints[53]
	assert.False(t, hasUDPHint)
	_, has443Hint := hints[443]
	assert.False(t, has443Hint)
}

func TestGetConnectionInfo_UsesHintedProtocolWhenProvided(t *testing.T) {
	containers := []ContainerInfo{
		{
			Publishers: []PublisherInfo{
				{Protocol: "tcp", PublishedPort: 40022, TargetPort: 22, URL: "0.0.0.0"},
				{Protocol: "tcp", PublishedPort: 40080, TargetPort: 80, URL: "0.0.0.0"},
			},
		},
	}

	conn, err := GetConnectionInfo(containers, "instancer.example.com", map[int]string{22: "ssh"})
	require.NoError(t, err)
	assert.Equal(t, "ssh://instancer.example.com:40022\ntcp://instancer.example.com:40080", conn)
}

func TestGetConnectionInfo_UsesDockerProtocolWhenNoHint(t *testing.T) {
	containers := []ContainerInfo{
		{
			Publishers: []PublisherInfo{
				{Protocol: "tcp", PublishedPort: 40022, TargetPort: 22, URL: "0.0.0.0"},
			},
		},
	}

	conn, err := GetConnectionInfo(containers, "instancer.example.com", nil)
	require.NoError(t, err)
	assert.Equal(t, "tcp://instancer.example.com:40022", conn)
}

func TestNormalizePublishedPorts_RandomizeHostPorts_WhenEnabled(t *testing.T) {
	params := map[string]interface{}{
		"published_ports": []interface{}{
			"22/ssh",
			"53/udp",
			"8080:80/http",
			443,
		},
	}

	normalized, hints, bindings := normalizePublishedPortsWithState(params, true, nil, true, defaultPortRange)
	ports, ok := normalized["published_ports"].([]interface{})
	require.True(t, ok)
	require.Len(t, ports, 4)

	first, ok := ports[0].(string)
	require.True(t, ok)
	assert.True(t, isRandomBindingFor(first, "22"))

	second, ok := ports[1].(string)
	require.True(t, ok)
	assert.True(t, isRandomBindingFor(second, "53/udp"))

	third, ok := ports[2].(string)
	require.True(t, ok)
	assert.Equal(t, "8080:80", third)

	fourth, ok := ports[3].(string)
	require.True(t, ok)
	assert.True(t, isRandomBindingFor(fourth, "443"))

	assert.Equal(t, "ssh", hints[22])
	assert.Equal(t, "http", hints[80])
	assert.NotEmpty(t, bindings)
}

var defaultPortRange = portRange{lo: config.DefaultRandomizedPortMin, hi: config.DefaultRandomizedPortMax}

func isRandomBindingFor(portDef string, target string) bool {
	parts := strings.Split(portDef, ":")
	if len(parts) != 2 {
		return false
	}
	if parts[1] != target {
		return false
	}
	hostPort, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	return defaultPortRange.contains(hostPort)
}
