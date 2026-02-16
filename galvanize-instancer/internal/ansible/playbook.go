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
