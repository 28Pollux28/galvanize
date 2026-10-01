package ansible

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"maps"
	"path"

	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/internal/docker"
	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/apenella/go-ansible/v2/pkg/execute"
	"github.com/apenella/go-ansible/v2/pkg/execute/configuration"
	jsonresults "github.com/apenella/go-ansible/v2/pkg/execute/result/json"
	"github.com/apenella/go-ansible/v2/pkg/execute/result/transformer"
	"github.com/apenella/go-ansible/v2/pkg/playbook"
)

// generateRandomID creates a short random ID for unique control paths
func generateRandomID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// BuildResourceOverrides converts merged ResourceLimits into a map suitable
// for Ansible's combine filter. The resulting dict is merged into each Docker
// Compose service definition by the playbooks.
//
// All limits, the PID limit included, go under deploy.resources.limits.
// Docker Compose 2.38 and later reject a service that sets the service-level
// pids_limit next to deploy.resources.limits ("can't set distinct values on
// 'pids_limit' and 'deploy.resources.limits.pids'"), which made every
// deployment with a PID limit and a CPU or memory limit fail.
func BuildResourceOverrides(rl config.ResourceLimits) map[string]interface{} {
	limits := map[string]interface{}{}
	if rl.CPUs != "" {
		limits["cpus"] = rl.CPUs
	}
	if rl.Memory != "" {
		limits["memory"] = rl.Memory
	}
	if rl.PidsLimit > 0 {
		limits["pids"] = rl.PidsLimit
	}

	overrides := map[string]interface{}{}
	if len(limits) > 0 {
		overrides["deploy"] = map[string]interface{}{
			"resources": map[string]interface{}{
				"limits": limits,
			},
		}
	}
	return overrides
}

func PreparePlaybook(conf *config.Config, tag string, challenge *challenge.Challenge, teamID string, params map[string]interface{}) (execute.Executor, *bytes.Buffer) {
	composeProject := docker.BuildComposeProject(challenge.Unique, challenge.Name, teamID)

	// Ansible playbook options
	playbookOpts := &playbook.AnsiblePlaybookOptions{
		ExtraVars: map[string]interface{}{
			"ansible_python_interpreter": "/usr/bin/python3",
			"deprecation_warnings":       "False",
			"compose_project":            composeProject,
			"domain_root":                conf.Instancer.InstancerHost,
			"team_id":                    teamID,
			"challenge_name":             challenge.Name,
			"env":                        params["env"],
		},
		Inventory:  conf.Instancer.Ansible.Inventory,
		Connection: "ssh",
		PrivateKey: conf.Instancer.Ansible.PrivateKey,
		User:       conf.Instancer.Ansible.User,
		Tags:       tag,
	}
	// Add deploy parameters to extra vars
	maps.Copy(playbookOpts.ExtraVars, params)
	maps.Copy(playbookOpts.ExtraVars, conf.Instancer.ExtraDeploymentParameters)

	// Merge resource limits (challenge overrides config defaults) and pass to Ansible
	merged := config.MergeResourceLimits(conf.Instancer.DefaultResourceLimits, challenge.ResourceLimits)
	if resourceOverrides := BuildResourceOverrides(merged); len(resourceOverrides) > 0 {
		playbookOpts.ExtraVars["resource_overrides"] = resourceOverrides
	}

	// Ensure env is always a map[string]interface{}.
	// If the challenge has no "env" key, params["env"] is nil. Go-ansible
	// serialises nil as JSON null, and Jinja2's default() filter only fires
	// for Undefined — not None — so "{{ env | default({}) }}" renders as an
	// empty string, which docker-compose rejects with "must be a mapping".
	if _, ok := playbookOpts.ExtraVars["env"].(map[string]interface{}); !ok {
		playbookOpts.ExtraVars["env"] = map[string]interface{}{}
	}

	pbCmd := playbook.NewAnsiblePlaybookCmd(
		playbook.WithPlaybooks(path.Join(conf.Instancer.AnsibleDir, challenge.PlaybookName+".yaml")),
		playbook.WithPlaybookOptions(playbookOpts),
	)

	// Small buffer - without verbose output, results are compact
	resultsBuff := bytes.NewBuffer(make([]byte, 0, 8*1024)) // 8KB initial capacity

	// SSH args with unique control path per playbook run to prevent shared socket issues
	// Add connection timeout (30s) and ServerAliveInterval to detect dead connections
	uniqueID := generateRandomID()
	sshArgs := "-o ControlMaster=auto -o ControlPersist=30s -o ControlPath=/tmp/ansible-ssh-%%h-%%p-%%r-" + uniqueID +
		" -o ConnectTimeout=30 -o ServerAliveInterval=10 -o ServerAliveCountMax=3"

	defaultExecutor := execute.NewDefaultExecute(
		execute.WithCmd(pbCmd),
		execute.WithErrorEnrich(playbook.NewAnsiblePlaybookErrorEnrich()),
		execute.WithWrite(resultsBuff),
		execute.WithTransformers(
			transformer.Prepend("ansible-playbook"),
		),
		execute.WithEnvVars(map[string]string{
			"ANSIBLE_SSH_ARGS": sshArgs,
		}),
	)
	defaultExecutor.Quiet()
	defaultExecutor.WithOutput(jsonresults.NewJSONStdoutCallbackResults())
	executor := configuration.NewAnsibleWithConfigurationSettingsExecute(
		defaultExecutor,
		configuration.WithAnsibleStdoutCallback("json"),
		configuration.WithoutAnsibleDeprecationWarnings(),
		configuration.WithAnsiblePipelining(),
		configuration.WithoutAnsibleHostKeyChecking(),
		configuration.WithAnsibleForks(1),         // Single fork per worker - parallelism handled by worker pool
		configuration.WithAnsibleTimeout(120),     // SSH connection timeout in seconds
		configuration.WithAnsibleTaskTimeout(300), // Task timeout: 5 minutes max per task
	)
	return executor, resultsBuff
}
