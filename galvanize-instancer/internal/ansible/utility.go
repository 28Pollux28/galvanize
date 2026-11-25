package ansible

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/apenella/go-ansible/v2/pkg/execute"
	"github.com/apenella/go-ansible/v2/pkg/execute/configuration"
	jsonresults "github.com/apenella/go-ansible/v2/pkg/execute/result/json"
	"github.com/apenella/go-ansible/v2/pkg/playbook"
)

// Galvanize's own playbooks (image builds and maintenance). Unlike the
// deployment playbooks of ansible_dir, which operators adapt, they are part
// of the binary.
//
//go:embed utility_playbooks/*.yaml
var utilityPlaybooks embed.FS

// Utility playbook names.
const (
	PlaybookBuild  = "build"
	PlaybookImages = "images"
	// PlaybookStop is run by Run itself after a cancelled or timed out run
	PlaybookStop = "stop"
)

// stopTimeout bounds the stop playbook run after a cancelled or timed out
// run, and stopTaskTimeout its task (stopScript waits 20 s for the
// processes to exit). With stopGrace, they bound how long stopping a run
// takes (StopDuration).
const (
	stopTimeout     = 40 * time.Second
	stopTaskTimeout = 30 * time.Second
)

// StopDuration is the longest a cancelled run takes to stop: ansible-playbook
// (stopGrace), then the stop playbook. A shutdown must leave that much time.
func StopDuration() time.Duration { return stopGrace + stopTimeout }

// stopScript stops what one run of a utility playbook left on a host.
// Stopping ansible-playbook does not stop the commands it started there
// (ssh does not signal them when the connection closes): a cancelled build
// went on and tagged its images. The playbooks set GALVANIZE_RUN_ID on
// their tasks and the processes they start inherit it, so the processes
// with the run's ID in their environment are terminated (killed after 20
// s), and the run's build directory, which its own cleanup task did not
// remove, is removed. $1 is the run ID.
const stopScript = `id=$1
case $id in ''|*[!0-9a-f]*) echo "invalid run ID" >&2; exit 2 ;; esac
marked() {
	for env in /proc/[0-9]*/environ; do
		if tr '\0' '\n' < "$env" 2>/dev/null | grep -qx "GALVANIZE_RUN_ID=$id"; then
			pid=${env#/proc/}
			echo "${pid%/environ}"
		fi
	done
}
pids=$(marked)
if [ -n "$pids" ]; then
	echo "stopping" $pids
	kill -TERM $pids 2>/dev/null
fi
n=0
while [ -n "$pids" ] && [ $n -lt 20 ]; do
	sleep 1
	n=$((n + 1))
	pids=$(marked)
done
if [ -n "$pids" ]; then
	echo "killing" $pids
	kill -KILL $pids 2>/dev/null
fi
rm -rf "${TMPDIR:-/tmp}/galvanize-build-$id-"*
`

// HostResult is the outcome of one task on one host.
type HostResult struct {
	Host        string
	Task        string
	Failed      bool
	Unreachable bool
	Skipped     bool
	Stdout      []string
	Stderr      []string
	Msg         string
}

// UtilityRunner runs a Galvanize utility playbook on every Ansible host.
type UtilityRunner interface {
	// Run returns the per-host results of every task, and an error when the
	// run failed (results are still returned when Ansible produced them).
	// When ctx is cancelled or times out, Run returns once the run is
	// stopped, on the hosts too.
	Run(ctx context.Context, conf *config.Config, name string, extraVars map[string]interface{}, taskTimeout time.Duration) ([]HostResult, error)
}

// AnsibleUtilityRunner is the production UtilityRunner.
type AnsibleUtilityRunner struct{}

var _ UtilityRunner = AnsibleUtilityRunner{}

func (AnsibleUtilityRunner) Run(ctx context.Context, conf *config.Config, name string, extraVars map[string]interface{}, taskTimeout time.Duration) ([]HostResult, error) {
	runID := generateRandomID()
	results, err := runUtility(ctx, conf, name, runID, extraVars, taskTimeout)
	// A run that returned no error ended: nothing of it is left to stop
	if err == nil || ctx.Err() == nil || name == PlaybookStop {
		return results, err
	}
	if stopErr := stopRun(ctx, conf, runID); stopErr != nil {
		return results, fmt.Errorf("%w; stopping it on the hosts failed, its commands may still run there: %v", err, stopErr)
	}
	return results, err
}

// stopRun runs the stop playbook for the run runID, which was cancelled or
// timed out (ctx is its context), within stopTimeout. The error names the
// hosts where it failed.
func stopRun(ctx context.Context, conf *config.Config, runID string) error {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	stopResults, stopErr := runUtility(stopCtx, conf, PlaybookStop, generateRandomID(), map[string]interface{}{
		"stop_script": stopScript,
		"stop_run_id": runID,
	}, stopTaskTimeout)
	if stopErr == nil {
		return nil
	}
	var hosts []string
	for _, r := range stopResults {
		if r.Failed || r.Unreachable {
			hosts = append(hosts, r.Host)
		}
	}
	if len(hosts) > 0 {
		return fmt.Errorf("%w (%s)", stopErr, strings.Join(hosts, ", "))
	}
	return stopErr
}

// runUtility runs one utility playbook. runID marks the run: Ansible's ssh
// control sockets, and GALVANIZE_RUN_ID on the hosts.
func runUtility(ctx context.Context, conf *config.Config, name, runID string, extraVars map[string]interface{}, taskTimeout time.Duration) ([]HostResult, error) {
	content, err := utilityPlaybooks.ReadFile("utility_playbooks/" + name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("unknown utility playbook %q", name)
	}
	dir, err := os.MkdirTemp("", "galvanize-playbook-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return nil, err
	}

	vars := map[string]interface{}{
		"ansible_python_interpreter": "/usr/bin/python3",
		"deprecation_warnings":       "False",
	}
	for k, v := range extraVars {
		vars[k] = Unsafe(v)
	}
	vars["galvanize_run_id"] = runID
	opts := &playbook.AnsiblePlaybookOptions{
		ExtraVars:  vars,
		Inventory:  conf.Instancer.Ansible.Inventory,
		Connection: "ssh",
		PrivateKey: conf.Instancer.Ansible.PrivateKey,
		User:       conf.Instancer.Ansible.User,
	}
	cmd := playbook.NewAnsiblePlaybookCmd(playbook.WithPlaybooks(path), playbook.WithPlaybookOptions(opts))

	out := bytes.NewBuffer(make([]byte, 0, 64*1024))
	// Raw JSON output (no line transformers), as the deployments read it.
	// TMPDIR: what a stopped ansible-playbook leaves in the temporary
	// directory (Python's multiprocessing directories) goes with dir.
	exec := execute.NewDefaultExecute(
		execute.WithCmd(cmd),
		execute.WithWrite(out),
		execute.WithEnvVars(map[string]string{"ANSIBLE_SSH_ARGS": SSHArgs(runID), "TMPDIR": dir}),
	)
	exe := withProcessExecutable(exec)
	exec.Quiet()
	exec.WithOutput(jsonresults.NewJSONStdoutCallbackResults())
	seconds := int(taskTimeout.Seconds())
	if seconds <= 0 {
		seconds = 300
	}
	runner := configuration.NewAnsibleWithConfigurationSettingsExecute(
		exec,
		configuration.WithAnsibleStdoutCallback("json"),
		configuration.WithoutAnsibleDeprecationWarnings(),
		configuration.WithAnsiblePipelining(),
		configuration.WithoutAnsibleHostKeyChecking(),
		configuration.WithAnsibleForks(10),
		configuration.WithAnsibleTimeout(120),
		configuration.WithAnsibleTaskTimeout(seconds),
	)
	runErr := cancellableExecutor{inner: runner, exe: exe}.Execute(ctx)
	results, parseErr := ParseHostResults(out.Bytes())
	// Cancelled, even when ansible-playbook exited 0 after the cancellation
	if ctx.Err() != nil {
		return results, fmt.Errorf("cancelled or timed out: %w", ctx.Err())
	}
	if runErr != nil {
		if parseErr != nil {
			return nil, fmt.Errorf("ansible-playbook failed (%s) and its output could not be read: %v", lastLine(runErr.Error()), parseErr)
		}
		// The per-host results say what failed
		return results, fmt.Errorf("ansible-playbook failed: %s", lastLine(runErr.Error()))
	}
	if parseErr != nil {
		return nil, parseErr
	}
	return results, nil
}

// Unsafe marks every string of v as unsafe for Ansible: the values come from
// challenge repositories (image and service names, folder names...), and
// Ansible renders Jinja in extra vars, so "{{ lookup('pipe', ...) }}" in an
// image name would run a command in Galvanize. Ansible reads
// {"__ansible_unsafe": s} in JSON extra vars as the string s, never
// rendered.
func Unsafe(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		return map[string]interface{}{"__ansible_unsafe": t}
	case []string:
		out := make([]interface{}, len(t))
		for i, s := range t {
			out[i] = Unsafe(s)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = Unsafe(e)
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, e := range t {
			out[k] = Unsafe(e)
		}
		return out
	}
	return v
}

// SSHArgs are the ssh options of one ansible-playbook run. The ControlPath
// has %C (a hash of the host, port and user) so that each host gets its own
// master connection: ANSIBLE_SSH_ARGS reaches ssh unformatted, so "%%h"
// would be a literal "%h" and every host would share the first host's
// socket, running the other hosts' tasks on it. id keeps runs apart.
func SSHArgs(id string) string {
	return "-o ControlMaster=auto -o ControlPersist=30s -o ControlPath=/tmp/ansible-ssh-%C-" + id +
		" -o ConnectTimeout=30 -o ServerAliveInterval=10 -o ServerAliveCountMax=3"
}

// ParseHostResults reads the per-host task results of Ansible's json
// stdout callback.
func ParseHostResults(data []byte) ([]HostResult, error) {
	var root struct {
		Plays []struct {
			Tasks []struct {
				Task struct {
					Name string `json:"name"`
				} `json:"task"`
				Hosts map[string]struct {
					Failed      bool            `json:"failed"`
					Unreachable bool            `json:"unreachable"`
					Skipped     bool            `json:"skipped"`
					StdoutLines []string        `json:"stdout_lines"`
					StderrLines []string        `json:"stderr_lines"`
					Msg         json.RawMessage `json:"msg"`
				} `json:"hosts"`
			} `json:"tasks"`
		} `json:"plays"`
	}
	// Warnings may come before the JSON document, which starts a line
	if i := bytes.Index(data, []byte("\n{")); i >= 0 && !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		data = data[i+1:]
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("no output from ansible-playbook")
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("cannot parse Ansible results: %w", err)
	}
	var out []HostResult
	for _, play := range root.Plays {
		for _, task := range play.Tasks {
			for host, r := range task.Hosts {
				out = append(out, HostResult{
					Host: host, Task: task.Task.Name, Failed: r.Failed, Unreachable: r.Unreachable,
					Skipped: r.Skipped, Stdout: r.StdoutLines, Stderr: r.StderrLines, Msg: rawMsg(r.Msg),
				})
			}
		}
	}
	return out, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func rawMsg(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}
