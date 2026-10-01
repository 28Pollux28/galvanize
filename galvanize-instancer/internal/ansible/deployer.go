package ansible

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/config"
	pkgerrors "github.com/28Pollux28/galvanize/pkg/errors"
	results "github.com/apenella/go-ansible/v2/pkg/execute/result/json"
	"go.uber.org/zap"
)

// maxRetries is the number of times to retry on transient errors.
const maxRetries = 3

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
// Concurrency is controlled by the worker pool; this deployer focuses on execution and retry logic.
type AnsibleDeployer struct{}

var _ Deployer = (*AnsibleDeployer)(nil)

func deploymentKey(chall *challenge.Challenge, teamID string) string {
	return chall.Category + "/" + chall.Name + ":" + teamID
}

func (a *AnsibleDeployer) Deploy(ctx context.Context, conf *config.Config, chall *challenge.Challenge, teamID string) (string, error) {
	var lastErr error
	key := deploymentKey(chall, teamID)
	randomizePorts := conf.Instancer.RandomizePublishedPorts
	ports := randomizedPortRange(conf)
	for attempt := 1; attempt <= maxRetries; attempt++ {
		existingBindings := loadPortBindingsFromDB(conf.Instancer.DBPath, key)
		if randomizePorts {
			randomizable := randomizableContainerPorts(chall.DeployParameters)
			randomizable = append(randomizable, exposeTCPContainerPorts(chall.DeployParameters)...)
			reserved, err := ensureRandomPortBindingsInDB(conf.Instancer.DBPath, key, randomizable, ports)
			if err != nil {
				return "", fmt.Errorf("failed to reserve host ports: %w", err)
			}
			existingBindings = reserved
		}
		normalizedParams, protocolHints, _ := normalizePublishedPortsWithState(chall.DeployParameters, randomizePorts, existingBindings, false, ports)
		if randomizePorts {
			savePortBindingsToDB(conf.Instancer.DBPath, key, existingBindings)
		}
		exposeHints, err := applyComposeExposures(conf, chall, teamID, normalizedParams, existingBindings)
		if err != nil {
			return "", fmt.Errorf("failed to apply compose exposures: %w", err)
		}
		for port, scheme := range exposeHints {
			protocolHints[port] = scheme
		}
		executor, resultsBuff := PreparePlaybook(conf, "create", chall, teamID, normalizedParams)

		if err := executor.Execute(ctx); err != nil {
			lastErr = err
			output := resultsBuff.String()
			// Clear buffer to help GC
			resultsBuff.Reset()
			// Check if it's a transient error and we should retry
			if isErr, errPattern := pkgerrors.IsTransientError(err, output); isErr && attempt < maxRetries {
				zap.S().Warnf("Transient error %s on deploy attempt %d/%d for team %s, retrying: %v", errPattern, attempt, maxRetries, teamID, err)
				time.Sleep(time.Duration(attempt) * 2 * time.Second) // Exponential backoff
				continue
			}
			// Parse output for logging before clearing
			logAnsibleErrorFromString(output, "deploy", err)
			return "", fmt.Errorf("ansible deploy failed: %w", err)
		}

		containerInfos, err := ExtractContainerInfo(resultsBuff)
		// Clear buffer immediately after extraction to free memory
		resultsBuff.Reset()
		if err != nil {
			return "", fmt.Errorf("failed to extract container info: %w", err)
		}
		if len(containerInfos) == 0 {
			return "", fmt.Errorf("no container info found in Ansible results")
		}

		connInfo, err := GetConnectionInfo(containerInfos, conf.Instancer.InstancerHost, protocolHints)
		if err != nil {
			return "", fmt.Errorf("failed to build connection info: %w", err)
		}

		return connInfo, nil
	}

	return "", fmt.Errorf("ansible deploy failed after %d attempts: %w", maxRetries, lastErr)
}

func (a *AnsibleDeployer) Terminate(ctx context.Context, conf *config.Config, chall *challenge.Challenge, teamID string) error {
	var lastErr error
	key := deploymentKey(chall, teamID)
	randomizePorts := conf.Instancer.RandomizePublishedPorts
	for attempt := 1; attempt <= maxRetries; attempt++ {
		existingBindings := loadPortBindingsFromDB(conf.Instancer.DBPath, key)
		normalizedParams, _, _ := normalizePublishedPortsWithState(chall.DeployParameters, randomizePorts, existingBindings, false, randomizedPortRange(conf))
		if _, err := applyComposeExposures(conf, chall, teamID, normalizedParams, existingBindings); err != nil {
			return fmt.Errorf("failed to apply compose exposures: %w", err)
		}
		executor, resultsBuff := PreparePlaybook(conf, "delete", chall, teamID, normalizedParams)

		if err := executor.Execute(ctx); err != nil {
			lastErr = err
			output := resultsBuff.String()
			// Clear buffer to help GC
			resultsBuff.Reset()
			// Check if it's a transient error and we should retry
			if isErr, errPattern := pkgerrors.IsTransientError(err, output); isErr && attempt < maxRetries {
				zap.S().Warnf("Transient error %s on terminate attempt %d/%d for team %s, retrying: %v", errPattern, attempt, maxRetries, teamID, err)
				time.Sleep(time.Duration(attempt) * 2 * time.Second) // Exponential backoff
				continue
			}
			// Parse output for logging before clearing
			logAnsibleErrorFromString(output, "terminate", err)
			return fmt.Errorf("ansible terminate failed: %w", err)
		}
		// Clear buffer to free memory
		resultsBuff.Reset()
		if randomizePorts {
			clearPortBindingsFromDB(conf.Instancer.DBPath, key)
		}
		return nil
	}

	return fmt.Errorf("ansible terminate failed after %d attempts: %w", maxRetries, lastErr)
}

// logAnsibleErrorFromString logs Ansible errors from a string (used when buffer is already converted)
func logAnsibleErrorFromString(output string, operation string, execErr error) {
	zap.S().Errorf("Ansible %s failed: %v", operation, execErr)
	res, err := results.ParseJSONResultsStream(bytes.NewBufferString(output))
	if err != nil {
		zap.S().Errorf("Failed to parse Ansible results: %v", err)
		return
	}
	errString := res.String()
	res = nil // Help GC
	zap.S().Errorf("Ansible %s fail reason: %s", operation, errString)
}
