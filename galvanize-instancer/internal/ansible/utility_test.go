package ansible

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/28Pollux28/galvanize/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseHostResults(t *testing.T) {
	out := []byte(`[WARNING]: a warning {with braces} before the JSON
{"plays":[{"tasks":[
 {"task":{"name":"Build"},"hosts":{
   "10.0.0.1":{"changed":true,"stdout_lines":["ok"],"stderr_lines":["#1 DONE"]},
   "10.0.0.2":{"failed":true,"msg":"non-zero return code","stderr_lines":["boom"]}}},
 {"task":{"name":"Remove build directory"},"hosts":{"10.0.0.3":{"unreachable":true,"msg":["a","b"]}}}
]}]}`)
	res, err := ParseHostResults(out)
	require.NoError(t, err)
	require.Len(t, res, 3)
	byHost := map[string]HostResult{}
	for _, r := range res {
		byHost[r.Host] = r
	}
	assert.Equal(t, []string{"#1 DONE"}, byHost["10.0.0.1"].Stderr)
	assert.True(t, byHost["10.0.0.2"].Failed)
	assert.Equal(t, "non-zero return code", byHost["10.0.0.2"].Msg)
	assert.True(t, byHost["10.0.0.3"].Unreachable)
	assert.Equal(t, `["a","b"]`, byHost["10.0.0.3"].Msg)

	_, err = ParseHostResults(nil)
	assert.Error(t, err)
}

func TestUtilityPlaybooksAreEmbedded(t *testing.T) {
	for _, name := range []string{PlaybookBuild, PlaybookImages, PlaybookStop} {
		data, err := utilityPlaybooks.ReadFile("utility_playbooks/" + name + ".yaml")
		require.NoError(t, err)
		assert.Contains(t, string(data), "hosts: all")
	}
}

// fakeAnsible puts an ansible-playbook on PATH that prints output and exits
// with code, and records its arguments.
func fakeAnsible(t *testing.T, output string, code int) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "output.json")
	require.NoError(t, os.WriteFile(out, []byte(output), 0o644))
	args := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + args + "\ncat " + out + "\nexit " + string(rune('0'+code)) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ansible-playbook"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return args
}

const buildOutput = `{"plays":[{"tasks":[{"task":{"name":"Build"},"hosts":{"h1":{"failed":true,"msg":"non-zero return code","stderr_lines":["#5 ERROR: boom"]}}}]}]}`

func TestRun_ReadsTheRawJSONOutput(t *testing.T) {
	// Regression: line transformers prefixed every line and broke the JSON
	args := fakeAnsible(t, buildOutput, 2)
	conf := &config.Config{Instancer: config.InstancerConfig{Ansible: config.AnsibleConfig{Inventory: "h1,", User: "u"}}}
	res, err := AnsibleUtilityRunner{}.Run(context.Background(), conf, PlaybookBuild, map[string]interface{}{"build_argv": []string{"docker", "build", "."}}, time.Minute)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "ANSIBLE_SSH_ARGS", "short error, not the whole command")
	require.Len(t, res, 1)
	assert.True(t, res[0].Failed)
	assert.Equal(t, []string{"#5 ERROR: boom"}, res[0].Stderr)

	recorded, _ := os.ReadFile(args)
	// Values from the repositories are never rendered as Jinja
	assert.Contains(t, string(recorded), `"build_argv":[{"__ansible_unsafe":"docker"},{"__ansible_unsafe":"build"},{"__ansible_unsafe":"."}]`)
	assert.Contains(t, string(recorded), `"ansible_python_interpreter":"/usr/bin/python3"`, "Galvanize's own variables stay plain")
	assert.Contains(t, string(recorded), "h1,")
}

func TestSSHArgs_OneMasterPerHost(t *testing.T) {
	args := SSHArgs("abcd")
	assert.Contains(t, args, "ControlPath=/tmp/ansible-ssh-%C-abcd")
	assert.NotContains(t, args, "%%", "ssh reads %% as a literal %: every host would share one socket")
}

func TestUnsafe(t *testing.T) {
	in := map[string]interface{}{
		"s":    "{{ lookup('pipe', 'id') }}",
		"list": []string{"a"},
		"any":  []interface{}{"b", 1, map[string]interface{}{"c": "d"}},
		"n":    3,
	}
	assert.Equal(t, map[string]interface{}{
		"s":    map[string]interface{}{"__ansible_unsafe": "{{ lookup('pipe', 'id') }}"},
		"list": []interface{}{map[string]interface{}{"__ansible_unsafe": "a"}},
		"any": []interface{}{map[string]interface{}{"__ansible_unsafe": "b"}, 1,
			map[string]interface{}{"c": map[string]interface{}{"__ansible_unsafe": "d"}}},
		"n": 3,
	}, Unsafe(in))
}

func TestRun_Success(t *testing.T) {
	fakeAnsible(t, strings.Replace(buildOutput, `"failed":true,`, "", 1), 0)
	conf := &config.Config{Instancer: config.InstancerConfig{Ansible: config.AnsibleConfig{Inventory: "h1,"}}}
	res, err := AnsibleUtilityRunner{}.Run(context.Background(), conf, PlaybookImages, nil, 0)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.False(t, res[0].Failed)
}

// slowAnsible puts an ansible-playbook on PATH that runs until it is
// stopped, running onTerm on SIGTERM, and answers the stop playbook with
// stopOutput and stopCode. It records its arguments and what happened in
// dir.
func slowAnsible(t *testing.T, onTerm, stopOutput string, stopCode int) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stop.json"), []byte(stopOutput), 0o644))
	script := `#!/bin/sh
d=` + dir + `
case "$*" in
*stop.yaml*) printf '%s\n' "$@" > $d/stop-args; cat $d/stop.json; exit ` + string(rune('0'+stopCode)) + ` ;;
esac
printf '%s\n' "$@" > $d/run-args
` + onTerm + `
echo started >> $d/events
sleep 30 >/dev/null 2>&1 &
child=$!
wait $child
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ansible-playbook"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// cancelWhenStarted cancels the context once the fake ansible-playbook runs.
func cancelWhenStarted(t *testing.T, dir string) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for i := 0; i < 500; i++ {
			if data, _ := os.ReadFile(filepath.Join(dir, "events")); strings.Contains(string(data), "started") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	return ctx
}

const stopOK = `{"plays":[{"tasks":[{"task":{"name":"Stop run"},"hosts":{"h1":{"changed":true}}}]}]}`

func TestRun_CancelStopsTheRunEverywhere(t *testing.T) {
	// Regression: cancelling SIGKILLed ansible-playbook only, its workers
	// and the build on the hosts went on, and Run returned at once
	dir := slowAnsible(t, `trap 'echo term >> $d/events; kill $child; sleep 0.3; echo exited >> $d/events; exit 1' TERM`, stopOK, 0)
	conf := &config.Config{Instancer: config.InstancerConfig{Ansible: config.AnsibleConfig{Inventory: "h1,"}}}
	_, err := AnsibleUtilityRunner{}.Run(cancelWhenStarted(t, dir), conf, PlaybookBuild, nil, time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cancelled")
	assert.NotContains(t, err.Error(), "stopping it on the hosts failed")

	events, _ := os.ReadFile(filepath.Join(dir, "events"))
	assert.Equal(t, "started\nterm\nexited\n", string(events), "SIGTERM, and Run waits until ansible-playbook exits")

	// The stop playbook ran for this run
	runArgs, _ := os.ReadFile(filepath.Join(dir, "run-args"))
	m := regexp.MustCompile(`"galvanize_run_id":"([0-9a-f]+)"`).FindStringSubmatch(string(runArgs))
	require.NotNil(t, m, string(runArgs))
	stopArgs, err := os.ReadFile(filepath.Join(dir, "stop-args"))
	require.NoError(t, err, "the stop playbook did not run")
	assert.Contains(t, string(stopArgs), `"stop_run_id":{"__ansible_unsafe":"`+m[1]+`"}`)
	assert.NotContains(t, string(stopArgs), `"galvanize_run_id":"`+m[1]+`"`, "the stop run has its own ID")
}

func TestRun_StopFailureIsReported(t *testing.T) {
	dir := slowAnsible(t, `trap 'kill $child; exit 1' TERM`,
		`{"plays":[{"tasks":[{"task":{"name":"Stop run"},"hosts":{"h1":{"unreachable":true,"msg":"no route"}}}]}]}`, 4)
	conf := &config.Config{Instancer: config.InstancerConfig{Ansible: config.AnsibleConfig{Inventory: "h1,"}}}
	_, err := AnsibleUtilityRunner{}.Run(cancelWhenStarted(t, dir), conf, PlaybookImages, nil, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cancelled")
	assert.Contains(t, err.Error(), "stopping it on the hosts failed")
	assert.Contains(t, err.Error(), "(h1)")
}

func TestRun_KillsTheRunOfAnAnsiblePlaybookThatIgnoresSIGTERM(t *testing.T) {
	// Regression: when ansible-playbook ignored SIGTERM, it was SIGKILLed
	// after stopGrace, but its workers (each leading its own session) went
	// on with its output open, and Run waited for them, the operation lock
	// held. The processes of the run are killed with it.
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("needs /proc")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("needs setsid")
	}
	old := stopGrace
	stopGrace = 200 * time.Millisecond
	t.Cleanup(func() { stopGrace = old })
	dir := slowAnsible(t, `trap '' TERM
setsid sh -c 'echo $$ > '$d'/worker; exec sleep 30' &`, stopOK, 0)
	conf := &config.Config{Instancer: config.InstancerConfig{Ansible: config.AnsibleConfig{Inventory: "h1,"}}}
	start := time.Now()
	_, err := AnsibleUtilityRunner{}.Run(cancelWhenStarted(t, dir), conf, PlaybookBuild, nil, time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cancelled")
	assert.Less(t, time.Since(start), 10*time.Second, "not after the worker's 30 s")
	data, err := os.ReadFile(filepath.Join(dir, "worker"))
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	// Killed: gone, or a zombie until init reaps it
	assert.Eventually(t, func() bool {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		return err != nil || strings.Contains(string(stat), ") Z ")
	}, 5*time.Second, 50*time.Millisecond, "the worker was not killed")
}

func TestRun_AnsiblePlaybookExiting0OnSIGTERM(t *testing.T) {
	// Regression: Wait then returned the context's error, which go-ansible
	// takes for an *exec.ExitError: a panic that stopped Galvanize (a build
	// runs in a goroutine of its own)
	dir := slowAnsible(t, `trap 'kill $child; exit 0' TERM`, stopOK, 0)
	conf := &config.Config{Instancer: config.InstancerConfig{Ansible: config.AnsibleConfig{Inventory: "h1,"}}}
	_, err := AnsibleUtilityRunner{}.Run(cancelWhenStarted(t, dir), conf, PlaybookBuild, nil, time.Minute)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "internal error", "no panic (recovered by cancellableExecutor)")
	assert.Contains(t, err.Error(), "cancelled", "a cancelled utility run is reported cancelled")
}

func TestRun_CancelledAfterAnsiblePlaybookExited0(t *testing.T) {
	// ansible-playbook printed its results and exited 0, a process it left
	// keeps its output open, and the context is cancelled meanwhile.
	// Regression: a panic (see above); then, once ansible-playbook was done,
	// "%!w(<nil>)" when stopping the run on the hosts failed.
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *stop.yaml*) echo '{\"plays\":[{\"tasks\":[{\"task\":{\"name\":\"Stop run\"},\"hosts\":{\"h1\":{\"unreachable\":true}}}]}]}'; exit 4;; esac\n" +
		"echo '" + stopOK + "'\n(sleep 1) &\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ansible-playbook"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	conf := &config.Config{Instancer: config.InstancerConfig{Ansible: config.AnsibleConfig{Inventory: "h1,"}}}
	_, err := AnsibleUtilityRunner{}.Run(ctx, conf, PlaybookImages, nil, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cancelled or timed out")
	assert.Contains(t, err.Error(), "stopping it on the hosts failed")
	assert.NotContains(t, err.Error(), "%!")
}

type panickingExecutor struct{}

func (panickingExecutor) Execute(context.Context) error { panic("interface conversion") }

func TestCancellableExecutor_RecoversPanics(t *testing.T) {
	err := cancellableExecutor{inner: panickingExecutor{}, exe: &processExecutable{}}.Execute(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal error running ansible-playbook: interface conversion")
}

func TestStopScript(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("needs /proc")
	}
	tmp := t.TempDir()
	start := func(id string) *exec.Cmd {
		cmd := exec.Command("sleep", "30")
		cmd.Env = append(os.Environ(), "GALVANIZE_RUN_ID="+id)
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}
	ours, other := start("abcd1234"), start("abcd12345")
	exited := make(chan struct{})
	go func() { _ = ours.Wait(); close(exited) }()
	buildDir := filepath.Join(tmp, "galvanize-build-abcd1234-x1")
	otherDir := filepath.Join(tmp, "galvanize-build-ffff0000-x2")
	require.NoError(t, os.MkdirAll(filepath.Join(buildDir, "ctx"), 0o755))
	require.NoError(t, os.MkdirAll(otherDir, 0o755))

	stop := exec.Command("sh", "-c", stopScript, "sh", "abcd1234")
	stop.Env = append(os.Environ(), "TMPDIR="+tmp)
	out, err := stop.CombinedOutput()
	require.NoError(t, err, string(out))
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the run's process was not stopped")
	}
	assert.NoError(t, other.Process.Signal(syscall.Signal(0)), "another run's process is left alone")
	assert.NoDirExists(t, buildDir)
	assert.DirExists(t, otherDir)

	out, err = exec.Command("sh", "-c", stopScript, "sh", "x; rm -rf /").CombinedOutput()
	assert.Error(t, err)
	assert.Contains(t, string(out), "invalid run ID")
}
