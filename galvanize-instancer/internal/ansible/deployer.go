package ansible

import (
	"bytes"
	"context"
	"fmt"

	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/config"
	results "github.com/apenella/go-ansible/v2/pkg/execute/result/json"
	"go.uber.org/zap"
)

// Deployer abstracts the Ansible deploy/terminate lifecycle so that
// handlers and models can be unit-tested without a real Ansible binary.
type Deployer interface {
	// Deploy runs the Ansible playbook that creates a challenge instance.
	// It returns the connection info string on success.
	Deploy(ctx context.Context, conf *config.Config, chall *challenge.Challenge, teamID string) (connectionInfo string, err error)

	// Terminate runs the Ansible playbook that destroys a challenge instance.
	Terminate(ctx context.Context, conf *config.Config, chall *challenge.Challenge, teamID string) error
}

// AnsibleDeployer is the production implementation of Deployer.
// It delegates to PreparePlaybook, ExtractContainerInfo, and GetConnectionInfo.
type AnsibleDeployer struct{}

var _ Deployer = (*AnsibleDeployer)(nil)

func (a *AnsibleDeployer) Deploy(ctx context.Context, conf *config.Config, chall *challenge.Challenge, teamID string) (string, error) {
	executor, resultsBuff := PreparePlaybook(conf, "create", chall, teamID, chall.DeployParameters)

	if err := executor.Execute(ctx); err != nil {
		logAnsibleError(resultsBuff, "deploy", err)
		return "", fmt.Errorf("ansible deploy failed: %w", err)
	}

	containerInfos, err := ExtractContainerInfo(resultsBuff)
	if err != nil {
		return "", fmt.Errorf("failed to extract container info: %w", err)
	}
	if len(containerInfos) == 0 {
		return "", fmt.Errorf("no container info found in Ansible results")
	}

	connInfo, err := GetConnectionInfo(containerInfos, conf.Instancer.InstancerHost)
	if err != nil {
		return "", fmt.Errorf("failed to build connection info: %w", err)
	}

	return connInfo, nil
}

func (a *AnsibleDeployer) Terminate(ctx context.Context, conf *config.Config, chall *challenge.Challenge, teamID string) error {
	executor, resultsBuff := PreparePlaybook(conf, "delete", chall, teamID, chall.DeployParameters)

	if err := executor.Execute(ctx); err != nil {
		logAnsibleError(resultsBuff, "terminate", err)
		return fmt.Errorf("ansible terminate failed: %w", err)
	}
	return nil
}

func logAnsibleError(resultsBuff *bytes.Buffer, operation string, execErr error) {
	zap.S().Errorf("Ansible %s failed: %v", operation, execErr)
	res, err := results.ParseJSONResultsStream(resultsBuff)
	if err != nil {
		zap.S().Errorf("Failed to parse Ansible results: %v", err)
		return
	}
	zap.S().Errorf("Ansible %s fail reason: %s", operation, res.String())
}
