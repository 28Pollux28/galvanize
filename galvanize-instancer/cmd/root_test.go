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

	setConfigDefaults(viper.GetViper())
	viper.SetConfigFile(path)
	require.NoError(t, viper.ReadInConfig())
	require.NoError(t, config.Load())

	ic := config.Get().Instancer
	assert.Equal(t, 3, ic.DeploymentMaxExtensions)
	assert.Equal(t, time.Hour, ic.DeploymentTTL)
	assert.Equal(t, 30*time.Minute, ic.DeploymentTTLExtension)
	assert.Equal(t, 30*time.Minute, ic.DeploymentExtensionWindow)
}

// A config file read while it is being written is not applied, and the
// complete file is applied once the writes stop
func TestReloadConfig_IgnoresAPartlyWrittenFile(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cfgFile = filepath.Join(t.TempDir(), "config.yaml")
	t.Cleanup(func() { cfgFile = "" })
	write := func(content string) { require.NoError(t, os.WriteFile(cfgFile, []byte(content), 0o600)) }
	full := func(host string) string {
		return "instancer:\n  instancer_host: " + host + "\n  challenge_dir: /c\n  ansible_dir: /a\n"
	}
	write(full("old.example.org"))
	viper.SetConfigFile(cfgFile)
	require.NoError(t, viper.ReadInConfig())
	require.NoError(t, config.Load())

	for _, partial := range []string{"", "instancer:\n  instancer_host: new.example.org\n"} {
		write(partial)
		reloadConfig(cfgFile)
		assert.Equal(t, "old.example.org", config.Get().Instancer.InstancerHost, "%q is not applied", partial)
	}

	write("")
	handleConfigChange(cfgFile)
	write(full("new.example.org"))
	handleConfigChange(cfgFile)
	require.Eventually(t, func() bool { return config.Get().Instancer.InstancerHost == "new.example.org" },
		5*reloadDelay, 10*time.Millisecond)
	assert.Equal(t, time.Hour, config.Get().Instancer.DeploymentTTL, "with the defaults")
}
