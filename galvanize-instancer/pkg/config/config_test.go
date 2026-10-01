package config

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRandomizedPortRange_Defaults(t *testing.T) {
	lo, hi := InstancerConfig{}.RandomizedPortRange()
	assert.Equal(t, 20000, lo)
	assert.Equal(t, 60999, hi)

	lo, hi = InstancerConfig{RandomizedPortMin: 30000}.RandomizedPortRange()
	assert.Equal(t, 30000, lo)
	assert.Equal(t, 60999, hi, "each bound defaults on its own")

	lo, hi = InstancerConfig{RandomizedPortMax: 25000}.RandomizedPortRange()
	assert.Equal(t, 20000, lo)
	assert.Equal(t, 25000, hi)
}

func TestInstancerConfig_Validate(t *testing.T) {
	for _, ok := range []InstancerConfig{
		{},
		{RandomizedPortMin: 30000, RandomizedPortMax: 30000},
		{RandomizedPortMin: 1, RandomizedPortMax: 65535},
	} {
		assert.NoError(t, ok.Validate(), "%+v", ok)
	}
	for _, bad := range []InstancerConfig{
		{RandomizedPortMin: 40000, RandomizedPortMax: 30000},
		{RandomizedPortMin: 61000}, // above the default maximum
		{RandomizedPortMax: 70000},
		{RandomizedPortMin: -1},
	} {
		assert.Error(t, bad.Validate(), "%+v", bad)
	}
}

func TestLoad_RejectsInvalidPortRange(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	viper.Set("instancer.randomized_port_min", 30000)
	viper.Set("instancer.randomized_port_max", 30009)
	require.NoError(t, Load())
	require.Equal(t, 30000, Get().Instancer.RandomizedPortMin)

	viper.Set("instancer.randomized_port_min", 40000)
	err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "randomized port range 40000-30009")
	assert.Equal(t, 30000, Get().Instancer.RandomizedPortMin, "the current config is kept")
}
