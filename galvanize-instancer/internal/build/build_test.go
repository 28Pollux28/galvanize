package build

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/28Pollux28/galvanize/internal/ansible"
	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/config"
	yaml "github.com/oasdiff/yaml3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func init() { zap.ReplaceGlobals(zap.NewNop()) }

type memLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *memLog) add(level, f string, a ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(f, a...))
}
func (l *memLog) Debugf(f string, a ...interface{})   { l.add("debug", f, a...) }
func (l *memLog) Infof(f string, a ...interface{})    { l.add("info", f, a...) }
func (l *memLog) Successf(f string, a ...interface{}) { l.add("success", f, a...) }
func (l *memLog) Warnf(f string, a ...interface{})    { l.add("warning", f, a...) }
func (l *memLog) Errorf(f string, a ...interface{})   { l.add("error", f, a...) }
func (l *memLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// load writes files under a challenge folder and indexes it like Galvanize.
func load(t *testing.T, files map[string]string) *challenge.Challenge {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "repo", "web", "chall")
	for name, content := range files {
		p := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	idx, err := challenge.NewChallengeIndex(base)
	require.NoError(t, err)
	require.Empty(t, idx.Skipped())
	cat := idx.Catalog()
	require.Len(t, cat, 1)
	return cat[0]
}

const httpChall = "name: Web App\ncategory: Web\ntype: zync\nplaybook_name: http\ndeploy_parameters:\n  image: webapp\n"

func TestPlan_ImageWithDockerfile(t *testing.T) {
	c := load(t, map[string]string{"challenge.yml": httpChall, "Dockerfile": "FROM busybox\n"})
	p := PlanFor(c)
	assert.Equal(t, KindImage, p.Kind)
	assert.Equal(t, []string{"webapp"}, p.Images)
	assert.Equal(t, []string{"docker", "build", "--progress=plain", "-t", "webapp", "-f", "Dockerfile", "."}, BuildArgv(c, p, false))
	assert.Contains(t, BuildArgv(c, p, true), "--no-cache")
}

func TestPlan_PulledImageHasNothingToBuild(t *testing.T) {
	// e.g. an image published to a registry, without Dockerfile
	c := load(t, map[string]string{"challenge.yml": httpChall})
	p := PlanFor(c)
	assert.Equal(t, KindNone, p.Kind)
	assert.Contains(t, p.Reason, "pulled")
}

func TestPlan_HTTPChallengeIgnoresASiblingComposeFile(t *testing.T) {
	// PolyPwnCTF "Nexus Labs 1": http playbook, compose file next to it
	c := load(t, map[string]string{"challenge.yml": httpChall, "Dockerfile": "FROM busybox\n",
		"docker-compose.yaml": "services:\n  app:\n    build: .\n"})
	assert.Equal(t, KindImage, PlanFor(c).Kind)
}

func TestPlan_ComposeFileServicesWithBuild(t *testing.T) {
	c := load(t, map[string]string{
		"challenge.yml": "name: Lab\ncategory: Web\ntype: zync\n",
		"compose.yml": "services:\n  web:\n    build: ./web\n  api:\n    build:\n      context: ./api\n    image: lab-api:1.2\n" +
			"  db:\n    image: mariadb:11\n",
	})
	p := PlanFor(c)
	require.Equal(t, KindCompose, p.Kind)
	assert.Equal(t, []string{"api", "web"}, p.Services)
	assert.Equal(t, []string{"lab-api:1.2", challenge.BuiltImageTag("Web", "Lab", "web")}, p.Images)

	var doc map[string]map[string]map[string]interface{}
	require.NoError(t, yaml.Unmarshal(p.composeDoc, &doc))
	assert.NotContains(t, doc["services"], "db", "only built services")
	assert.Equal(t, challenge.BuiltImageTag("Web", "Lab", "web"), doc["services"]["web"]["image"])

	argv := BuildArgv(c, p, false)
	assert.Equal(t, []string{"docker", "compose", "--progress", "plain", "-f", buildComposeName, "-p", "galvanize-build-" + challenge.BuildSlug("Web", "Lab"), "build", "api", "web"}, argv)

	// The deployment uses the same image name for the untagged service
	def := c.DeployParameters["compose_definition"].(string)
	assert.Contains(t, def, challenge.BuiltImageTag("Web", "Lab", "web"))
	assert.Contains(t, def, "lab-api:1.2")
}

func TestPlan_InlineDefinitionBuildsFromTheComposeFile(t *testing.T) {
	// PolyPwnCTF "Vault13": inline compose_definition with prebuilt images,
	// docker-compose.yml with the build sections next to it
	c := load(t, map[string]string{
		"challenge.yml": "name: Vault\ncategory: Web\ntype: zync\nplaybook_name: custom_compose\ndeploy_parameters:\n" +
			"  compose_definition: |\n    services:\n      haproxy:\n        image: vault-haproxy:latest\n",
		"docker-compose.yml": "services:\n  haproxy:\n    build: ./haproxy\n    image: vault-haproxy:latest\n",
	})
	p := PlanFor(c)
	require.Equal(t, KindCompose, p.Kind)
	assert.Equal(t, []string{"vault-haproxy:latest"}, p.Images)
}

func TestPlan_InlineMappingWithoutBuilds(t *testing.T) {
	// PolyPwnCTF "CatLab 1": every image comes from a registry
	c := load(t, map[string]string{
		"challenge.yml": "name: Cat\ncategory: Web\ntype: zync\nplaybook_name: custom_compose\ndeploy_parameters:\n" +
			"  compose_definition:\n    services:\n      web:\n        image: ghcr.io/org/cat:latest\n",
	})
	p := PlanFor(c)
	assert.Equal(t, KindNone, p.Kind)
	assert.Equal(t, []string{"ghcr.io/org/cat:latest"}, p.Images)
}

func TestPlan_SameNamesInTwoCategoriesDoNotCollide(t *testing.T) {
	assert.NotEqual(t, challenge.BuiltImageTag("Web", "login", "app"), challenge.BuiltImageTag("Pwn", "login", "app"))
}

func readArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		data, _ := io.ReadAll(tr)
		out[hdr.Name] = string(data)
		if hdr.Typeflag == tar.TypeSymlink {
			out[hdr.Name] = "-> " + hdr.Linkname
		}
	}
	return out
}

func TestArchive_ContentAndRefusals(t *testing.T) {
	c := load(t, map[string]string{
		"challenge.yml": "name: Lab\ncategory: Web\ntype: zync\n",
		"compose.yml":   "services:\n  web:\n    build: .\n",
		"Dockerfile":    "FROM busybox\n",
		".git/config":   "[core]\n",
		"src/app.py":    "print(1)\n",
	})
	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(c.Dir, "escape")))
	require.NoError(t, os.Symlink("src/app.py", filepath.Join(c.Dir, "inside")))

	log := &memLog{}
	plan := PlanFor(c)
	path, workdir, err := writeArchive(c, plan, log)
	require.NoError(t, err)
	defer func() { _ = os.Remove(path) }()
	files := readArchive(t, path)

	assert.Equal(t, "ctx", workdir)
	assert.Contains(t, files, "ctx/Dockerfile")
	assert.Equal(t, "print(1)\n", files["ctx/src/app.py"])
	assert.Contains(t, files, "ctx/"+buildComposeName)
	assert.Equal(t, "-> src/app.py", files["ctx/inside"])
	assert.NotContains(t, files, "ctx/escape", "symlink leading outside skipped")
	for name := range files {
		assert.NotContains(t, name, ".git/", "no .git")
	}
	assert.Contains(t, log.text(), "Skipped symlink")
}

// fakeRunner records the runs and answers with canned results.
type fakeRunner struct {
	mu       sync.Mutex
	calls    []map[string]interface{}
	active   int32
	maxSeen  int32
	delay    time.Duration
	respond  func(vars map[string]interface{}) ([]ansible.HostResult, error)
	archives []map[string]string
	t        *testing.T
}

func (f *fakeRunner) Run(ctx context.Context, conf *config.Config, name string, vars map[string]interface{}, _ time.Duration) ([]ansible.HostResult, error) {
	n := atomic.AddInt32(&f.active, 1)
	for {
		m := atomic.LoadInt32(&f.maxSeen)
		if n <= m || atomic.CompareAndSwapInt32(&f.maxSeen, m, n) {
			break
		}
	}
	defer atomic.AddInt32(&f.active, -1)
	time.Sleep(f.delay)
	f.mu.Lock()
	call := map[string]interface{}{"playbook": name}
	for k, v := range vars {
		call[k] = v
	}
	f.calls = append(f.calls, call)
	if a, ok := vars["build_archive"].(string); ok && f.t != nil {
		f.archives = append(f.archives, readArchive(f.t, a))
	}
	f.mu.Unlock()
	return f.respond(vars)
}

func ok(hosts ...string) func(map[string]interface{}) ([]ansible.HostResult, error) {
	return func(map[string]interface{}) ([]ansible.HostResult, error) {
		var out []ansible.HostResult
		for _, h := range hosts {
			out = append(out, ansible.HostResult{Host: h, Task: "Build"})
		}
		return out, nil
	}
}

func confWith(concurrency int) *config.Config {
	return &config.Config{Instancer: config.InstancerConfig{Build: config.BuildConfig{Concurrency: concurrency}}}
}

func TestBuild_SuccessFailureAndSkip(t *testing.T) {
	good := load(t, map[string]string{"challenge.yml": httpChall, "Dockerfile": "FROM busybox\n"})
	bad := load(t, map[string]string{"challenge.yml": strings.Replace(httpChall, "Web App", "Broken", 1), "Dockerfile": "FROM busybox\n"})
	pulled := load(t, map[string]string{"challenge.yml": strings.Replace(httpChall, "Web App", "Pulled", 1)})

	runner := &fakeRunner{t: t}
	calls := int32(0)
	runner.respond = func(vars map[string]interface{}) ([]ansible.HostResult, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return []ansible.HostResult{{Host: "10.0.0.1", Task: "Build"}, {Host: "10.0.0.2", Task: "Build"}}, nil
		}
		return []ansible.HostResult{{Host: "10.0.0.1", Task: "Build", Failed: true, Msg: "non-zero return code",
			Stderr: []string{"#5 ERROR: process did not complete successfully"}}}, errors.New("exit 2")
	}
	log := &memLog{}
	b := &Builder{Runner: runner}
	res := b.Build(context.Background(), confWith(1), []*challenge.Challenge{good, bad, pulled}, nil, false, log)

	assert.Len(t, res.Built, 1)
	assert.Len(t, res.Failed, 1)
	assert.Equal(t, []*challenge.Challenge{pulled}, res.Skipped)
	assert.Contains(t, log.text(), "on 10.0.0.1, 10.0.0.2")
	assert.Contains(t, log.text(), "#5 ERROR", "the failed build's output is logged")
	require.Len(t, runner.archives, 2)
	for _, a := range runner.archives {
		assert.Contains(t, a, "ctx/Dockerfile")
	}
}

func TestBuild_ErrorWithoutHostResultIsLogged(t *testing.T) {
	// Regression: a cancelled build (no results) only logged "Cancelled",
	// not why, nor that stopping it on the hosts failed
	c := load(t, map[string]string{"challenge.yml": httpChall, "Dockerfile": "FROM busybox\n"})
	runner := &fakeRunner{respond: func(map[string]interface{}) ([]ansible.HostResult, error) {
		return nil, errors.New("cancelled or timed out: context canceled; stopping it on the hosts failed")
	}}
	log := &memLog{}
	res := (&Builder{Runner: runner}).Build(context.Background(), confWith(1), []*challenge.Challenge{c}, nil, false, log)
	assert.Len(t, res.Failed, 1)
	assert.Contains(t, log.text(), "Build failed: cancelled or timed out: context canceled; stopping it on the hosts failed")
}

func TestBuild_PanicFailsTheBuild(t *testing.T) {
	// Regression: a panic in a build goroutine (out of reach of the
	// operation's recover) stopped Galvanize
	c := load(t, map[string]string{"challenge.yml": httpChall, "Dockerfile": "FROM busybox\n"})
	runner := &fakeRunner{respond: func(map[string]interface{}) ([]ansible.HostResult, error) { panic("boom") }}
	log := &memLog{}
	res := (&Builder{Runner: runner}).Build(context.Background(), confWith(1), []*challenge.Challenge{c}, nil, false, log)
	assert.Len(t, res.Failed, 1)
	assert.Contains(t, log.text(), "Build failed: internal error: boom")
}

func TestBuild_RespectsConcurrency(t *testing.T) {
	var challs []*challenge.Challenge
	for i := 0; i < 5; i++ {
		challs = append(challs, load(t, map[string]string{"challenge.yml": strings.Replace(httpChall, "Web App", fmt.Sprintf("App %d", i), 1), "Dockerfile": "FROM x\n"}))
	}
	runner := &fakeRunner{delay: 30 * time.Millisecond, respond: ok("h")}
	res := (&Builder{Runner: runner}).Build(context.Background(), confWith(2), challs, nil, false, &memLog{})
	assert.Len(t, res.Built, 5)
	assert.LessOrEqual(t, atomic.LoadInt32(&runner.maxSeen), int32(2))
	assert.Equal(t, int32(2), atomic.LoadInt32(&runner.maxSeen))
}

func TestImages_StatusAndRemove(t *testing.T) {
	built := load(t, map[string]string{"challenge.yml": httpChall, "Dockerfile": "FROM x\n"})
	pulled := load(t, map[string]string{"challenge.yml": strings.Replace(strings.Replace(httpChall, "Web App", "Pulled", 1), "webapp", "ghcr.io/org/p:1", 1)})

	runner := &fakeRunner{respond: func(vars map[string]interface{}) ([]ansible.HostResult, error) {
		images := vars["images"].([]string)
		var lines []string
		for _, i := range images {
			if i == "webapp" {
				lines = append(lines, "present "+i)
			} else {
				lines = append(lines, "absent "+i)
			}
		}
		return []ansible.HostResult{{Host: "h1", Task: "Images", Stdout: lines}}, nil
	}}
	b := &Builder{Runner: runner}
	res, err := b.Images(context.Background(), confWith(1), []*challenge.Challenge{built, pulled}, nil, ImagesStatus, &memLog{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"h1": "present"}, res.States["webapp"])
	assert.Equal(t, map[string]string{"h1": "absent"}, res.States["ghcr.io/org/p:1"], "status covers pulled images")
	assert.Equal(t, []string{"webapp"}, res.Challenges[built])

	runner.calls = nil
	_, err = b.Images(context.Background(), confWith(1), []*challenge.Challenge{built, pulled}, nil, ImagesRemove, &memLog{})
	require.NoError(t, err)
	got := runner.calls[0]["images"].([]string)
	sort.Strings(got)
	assert.Equal(t, []string{"webapp"}, got, "pulled images are never removed")
	assert.Contains(t, runner.calls[0]["images_script"], "docker image rm")

	_, err = b.Images(context.Background(), confWith(1), nil, nil, "explode", &memLog{})
	assert.Error(t, err)
}

func TestImages_HostsTheScriptDidNotRunOn(t *testing.T) {
	built := load(t, map[string]string{"challenge.yml": httpChall, "Dockerfile": "FROM x\n"})
	runner := &fakeRunner{respond: func(map[string]interface{}) ([]ansible.HostResult, error) {
		return []ansible.HostResult{
			{Host: "h1", Task: "Images", Stdout: []string{"present webapp"}},
			{Host: "h2", Task: "Images", Unreachable: true, Msg: "ssh: connect to host h2: timed out"},
			{Host: "h3", Task: "Images", Failed: true, Msg: "docker: not found"},
		}, errors.New("exit 4")
	}}
	log := &memLog{}
	res, err := (&Builder{Runner: runner}).Images(context.Background(), confWith(1), []*challenge.Challenge{built}, nil, ImagesStatus, log)
	require.NoError(t, err, "results came back: the hosts that answered are reported")
	assert.Equal(t, map[string]string{"h1": "present", "h2": StateUnreachable, "h3": StateError}, res.States["webapp"])
	assert.Contains(t, log.text(), "timed out")
}

func TestPlan_ComposeFileIsTheOneTheIndexFound(t *testing.T) {
	// The build reads the Compose file the index checked, and never looks
	// for another one
	inline := "name: Vault\ncategory: Web\ntype: zync\nplaybook_name: custom_compose\ndeploy_parameters:\n" +
		"  compose_file: build/compose.yml\n  compose_definition: |\n    services:\n      app:\n        image: vault-app\n"
	c := load(t, map[string]string{
		"challenge.yml":     inline,
		"build/compose.yml": "services:\n  app:\n    build: ../src\n    image: vault-app\n",
		"src/Dockerfile":    "FROM busybox\n",
	})
	p := PlanFor(c)
	require.Equal(t, KindCompose, p.Kind, p.Reason)
	assert.Equal(t, filepath.Join(c.Dir, "build"), p.composeDir)

	pulled := load(t, map[string]string{"challenge.yml": strings.Replace(inline, "  compose_file: build/compose.yml\n", "", 1)})
	require.NoError(t, os.WriteFile(filepath.Join(pulled.Dir, "compose.yml"), []byte("services:\n  app:\n    build: .\n"), 0o644))
	assert.Equal(t, KindNone, PlanFor(pulled).Kind, "a file added after indexing is not read")
}
