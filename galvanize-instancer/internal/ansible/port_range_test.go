package ansible

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRandomizedPortRange_FromConfig(t *testing.T) {
	conf := &config.Config{}
	assert.Equal(t, portRange{lo: 20000, hi: 60999}, randomizedPortRange(conf), "defaults")

	conf.Instancer.RandomizedPortMin = 30000
	conf.Instancer.RandomizedPortMax = 30009
	assert.Equal(t, portRange{lo: 30000, hi: 30009}, randomizedPortRange(conf))
}

func TestPortRange_Random(t *testing.T) {
	r := portRange{lo: 30000, hi: 30002}
	seen := map[int]bool{}
	for range 300 {
		p := r.random()
		require.True(t, r.contains(p), p)
		seen[p] = true
	}
	assert.Len(t, seen, 3, "every port of the range is picked")

	assert.Equal(t, 40000, portRange{lo: 40000, hi: 40000}.random())
	assert.Zero(t, portRange{lo: 2, hi: 1}.random(), "empty range")
}

func TestNormalizePublishedPorts_UsesConfiguredRange(t *testing.T) {
	params := map[string]interface{}{"published_ports": []interface{}{"22", "53/udp", 443}}
	r := portRange{lo: 31000, hi: 31002}

	normalized, _, bindings := normalizePublishedPortsWithState(params, true, nil, true, r)
	require.Len(t, bindings, 3)
	used := map[int]bool{}
	for _, def := range normalized["published_ports"].([]interface{}) {
		host, err := strconv.Atoi(strings.SplitN(def.(string), ":", 2)[0])
		require.NoError(t, err)
		assert.True(t, r.contains(host), def)
		used[host] = true
	}
	assert.Len(t, used, 3, "distinct host ports")
}

func TestEnsureRandomPortBindingsInDB_UsesConfiguredRange(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "galvanize.db")
	r := portRange{lo: 32000, hi: 32004}

	bindings, err := ensureRandomPortBindingsInDB(dbPath, "web/a:team1", []string{"22", "80"}, r)
	require.NoError(t, err)
	require.Len(t, bindings, 2)
	for _, p := range bindings {
		assert.True(t, r.contains(p), p)
	}
	assert.NotEqual(t, bindings["22"], bindings["80"])

	// Asking again returns the same reservation
	again, err := ensureRandomPortBindingsInDB(dbPath, "web/a:team1", []string{"22", "80"}, r)
	require.NoError(t, err)
	assert.Equal(t, bindings, again)
}

func TestEnsureRandomPortBindingsInDB_FillsRangeThenFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "galvanize.db")
	r := portRange{lo: 33000, hi: 33004}

	// Five deployments use up the five ports, the later ones through the
	// in-order fallback once random picks keep colliding
	used := map[int]bool{}
	for i := range 5 {
		b, err := ensureRandomPortBindingsInDB(dbPath, fmt.Sprintf("web/a:team%d", i), []string{"22"}, r)
		require.NoError(t, err)
		require.True(t, r.contains(b["22"]), b["22"])
		used[b["22"]] = true
	}
	assert.Len(t, used, 5)

	_, err := ensureRandomPortBindingsInDB(dbPath, "web/a:team5", []string{"22"}, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "33000-33004")

	// Freeing a deployment's bindings makes its port available again
	clearPortBindingsFromDB(dbPath, "web/a:team0")
	b, err := ensureRandomPortBindingsInDB(dbPath, "web/a:team5", []string{"22"}, r)
	require.NoError(t, err)
	assert.True(t, r.contains(b["22"]))
}
