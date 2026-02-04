package ansible

import (
	"bytes"
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
		Inventory:   conf.Instancer.Ansible.Inventory,
		Connection:  "ssh",
		PrivateKey:  conf.Instancer.Ansible.PrivateKey,
		User:        conf.Instancer.Ansible.User,
		VerboseVVVV: true,
		Tags:        tag,
	}
	// Add deploy parameters to extra vars
	maps.Copy(playbookOpts.ExtraVars, params)
	maps.Copy(playbookOpts.ExtraVars, conf.Instancer.ExtraDeploymentParameters)

	pbCmd := playbook.NewAnsiblePlaybookCmd(
		playbook.WithPlaybooks(path.Join(conf.Instancer.AnsibleDir, challenge.PlaybookName+".yaml")),
		playbook.WithPlaybookOptions(playbookOpts),
	)

	resultsBuff := bytes.NewBuffer([]byte{})

	defaultExecutor := execute.NewDefaultExecute(
		execute.WithCmd(pbCmd),
		execute.WithErrorEnrich(playbook.NewAnsiblePlaybookErrorEnrich()),
		execute.WithWrite(resultsBuff),
		execute.WithTransformers(
			transformer.Prepend("ansible-playbook"),
		),
	)
	defaultExecutor.Quiet()
	defaultExecutor.WithOutput(jsonresults.NewJSONStdoutCallbackResults())
	executor := configuration.NewAnsibleWithConfigurationSettingsExecute(
		defaultExecutor,
		configuration.WithAnsibleStdoutCallback("json"),
		configuration.WithoutAnsibleDeprecationWarnings(),
	)
	return executor, resultsBuff
}
