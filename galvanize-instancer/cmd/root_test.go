package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A config that omits the deployment settings gets the defaults documented
// in config.example.yaml
func TestSetConfigDefaults(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("instancer:\n  instancer_host: example.org\n"), 0o600))

	setConfigDefaults()
	viper.SetConfigFile(path)
	require.NoError(t, viper.ReadInConfig())
	require.NoError(t, config.Load())

	ic := config.Get().Instancer
	assert.Equal(t, 3, ic.DeploymentMaxExtensions)
	assert.Equal(t, time.Hour, ic.DeploymentTTL)
	assert.Equal(t, 30*time.Minute, ic.DeploymentTTLExtension)
	assert.Equal(t, 30*time.Minute, ic.DeploymentExtensionWindow)
}
