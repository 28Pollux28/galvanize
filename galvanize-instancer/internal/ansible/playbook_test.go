package ansible

import (
	"testing"

	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/stretchr/testify/assert"
)

func limitsOf(overrides map[string]interface{}) map[string]interface{} {
	deploy, _ := overrides["deploy"].(map[string]interface{})
	resources, _ := deploy["resources"].(map[string]interface{})
	limits, _ := resources["limits"].(map[string]interface{})
	return limits
}

func TestBuildResourceOverrides_AllLimitsUnderDeployResources(t *testing.T) {
	overrides := BuildResourceOverrides(config.ResourceLimits{CPUs: "1", Memory: "512M", PidsLimit: 256})

	assert.Equal(t, map[string]interface{}{"cpus": "1", "memory": "512M", "pids": 256}, limitsOf(overrides))
	// Docker Compose 2.38+ rejects pids_limit next to deploy.resources.limits
	assert.NotContains(t, overrides, "pids_limit")
	assert.Len(t, overrides, 1)
}

func TestBuildResourceOverrides_PidsOnly(t *testing.T) {
	overrides := BuildResourceOverrides(config.ResourceLimits{PidsLimit: 128})

	assert.Equal(t, map[string]interface{}{"pids": 128}, limitsOf(overrides))
	assert.NotContains(t, overrides, "pids_limit")
}

func TestBuildResourceOverrides_NoLimits(t *testing.T) {
	assert.Empty(t, BuildResourceOverrides(config.ResourceLimits{}))
}

func TestBuildResourceOverrides_MergedChallengeOverride(t *testing.T) {
	merged := config.MergeResourceLimits(
		config.ResourceLimits{CPUs: "1", Memory: "512M", PidsLimit: 256},
		config.ResourceLimits{PidsLimit: 512},
	)

	assert.Equal(t, map[string]interface{}{"cpus": "1", "memory": "512M", "pids": 512}, limitsOf(BuildResourceOverrides(merged)))
}
