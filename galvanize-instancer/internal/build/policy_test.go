package build

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/28Pollux28/galvanize/internal/ansible"
	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const composeChall = "name: Lab\ncategory: Web\ntype: zync\n"

func TestPlan_RefusesUnsafeComposeBuildSections(t *testing.T) {
	for name, build := range map[string]string{
		"host network":        "      context: .\n      network: host\n",
		"secrets":             "      context: .\n      secrets: [token]\n",
		"ssh agent":           "      context: .\n      ssh: [default]\n",
		"extra contexts":      "      context: .\n      additional_contexts:\n        etc: /etc\n",
		"extra tags":          "      context: .\n      tags: [traefik:v3.7]\n",
		"host cache":          "      context: .\n      cache_from: [\"type=local,src=/root\"]\n",
		"absolute context":    "      context: /etc\n",
		"parent context":      "      context: ../other\n",
		"remote context":      "      context: https://github.com/org/repo.git\n",
		"git context":         "      context: git@github.com:org/repo.git\n",
		"absolute dockerfile": "      context: .\n      dockerfile: /etc/Dockerfile\n",
		"parent dockerfile":   "      context: .\n      dockerfile: ../../Dockerfile\n",
	} {
		t.Run(name, func(t *testing.T) {
			c := load(t, map[string]string{"challenge.yml": composeChall, "compose.yml": "services:\n  web:\n    build:\n" + build})
			p := PlanFor(c)
			assert.True(t, p.Refused, p.Reason)
			assert.Equal(t, KindNone, p.Kind)
		})
	}

	c := load(t, map[string]string{"challenge.yml": composeChall, "compose.yml": "services:\n  web:\n    build:\n      context: ./app\n      dockerfile: Dockerfile.prod\n      args:\n        FLAG: x\n      target: run\n"})
	p := PlanFor(c)
	assert.Equal(t, KindCompose, p.Kind, p.Reason)
}

func TestPlan_RefusesNamesThatAreNotPlainNames(t *testing.T) {
	for _, image := range []string{"{{ lookup('pipe', 'id') }}", "--privileged", "UPPER/case", "a b", "x:" + strings.Repeat("t", 200)} {
		c := load(t, map[string]string{"challenge.yml": strings.Replace(httpChall, "webapp", strconv.Quote(image), 1), "Dockerfile": "FROM x\n"})
		assert.True(t, PlanFor(c).Refused, image)
	}
	c := load(t, map[string]string{"challenge.yml": composeChall, "compose.yml": "services:\n  \"-x\":\n    build: .\n"})
	assert.True(t, PlanFor(c).Refused, "service name starting with a dash")

	for _, ok := range []string{"webapp", "nssha-1", "rabb-it-hol:latest", "ghcr.io/polycyber/polypwn2026/up:latest", "localhost:5000/a/b:1.0", "a@sha256:" + strings.Repeat("0", 64)} {
		assert.True(t, ValidImageRef(ok), ok)
	}
}

func TestPlan_ComposeFileMustBuildWhatTheInlineDefinitionDeploys(t *testing.T) {
	inline := composeChall + "deploy_parameters:\n  compose_definition: |\n    services:\n      web:\n        image: lab-web:2\n"
	c := load(t, map[string]string{"challenge.yml": inline, "compose.yml": "services:\n  web:\n    build: .\n    image: lab-web:1\n"})
	c.PlaybookName = "custom_compose"
	p := PlanFor(c)
	require.True(t, p.Refused)
	assert.Contains(t, p.Reason, "builds lab-web:1 but the deployment")

	same := composeChall + "deploy_parameters:\n  compose_definition: |\n    services:\n      web:\n        image: lab-web\n"
	c = load(t, map[string]string{"challenge.yml": same, "compose.yml": "services:\n  web:\n    build: .\n    image: lab-web:latest\n"})
	c.PlaybookName = "custom_compose"
	assert.Equal(t, KindCompose, PlanFor(c).Kind, "lab-web and lab-web:latest are the same image")
}

func TestNormalizeAndProtected(t *testing.T) {
	assert.Equal(t, "nginx:latest", NormalizeImage("docker.io/library/nginx"))
	assert.Equal(t, "nginx:alpine", NormalizeImage("nginx:alpine"))
	assert.Equal(t, "localhost:5000/a:latest", NormalizeImage("localhost:5000/a"))
	assert.True(t, Protected("traefik:v3.7", DefaultProtectedImages))
	assert.True(t, Protected("docker.io/library/mariadb", DefaultProtectedImages))
	assert.True(t, Protected("ghcr.io/28pollux28/galvanize:0.7.5", DefaultProtectedImages))
	assert.False(t, Protected("traefik-chall:1", DefaultProtectedImages))
	assert.False(t, Protected("polypwn/redis-lab", DefaultProtectedImages))
}

// catalogOf indexes several challenge folders under one challenge_dir.
func catalogOf(t *testing.T, folders map[string]map[string]string) []*challenge.Challenge {
	t.Helper()
	base := t.TempDir()
	for dir, files := range folders {
		for name, content := range files {
			p := filepath.Join(base, dir, name)
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
			require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		}
	}
	idx, err := challenge.NewChallengeIndex(base)
	require.NoError(t, err)
	return idx.Catalog()
}

func TestPlanIn_ImagesOfOtherChallengeFolders(t *testing.T) {
	chall := func(name, image string) string {
		return "name: " + name + "\ncategory: Web\ntype: zync\nplaybook_name: http\ndeploy_parameters:\n  image: " + image + "\n"
	}
	catalog := catalogOf(t, map[string]map[string]string{
		"a/web/one":   {"challenge.yml": chall("One", "shared:1"), "Dockerfile": "FROM x\n"},
		"a/web/two":   {"challenge.yml": chall("Two", "shared:1"), "Dockerfile": "FROM x\n"},
		"b/web/base":  {"challenge.yml": chall("Base", "python:3.12-slim"), "Dockerfile": "FROM x\n"},
		"b/web/app":   {"challenge.yml": chall("App", "app:1"), "Dockerfile": "FROM python:3.12-slim AS build\nFROM build\nFROM ${BASE}\n"},
		"c/web/multi": {"challenge-1.yml": chall("M1", "multi:1"), "challenge-2.yml": chall("M2", "multi:1"), "Dockerfile": "FROM x\n"},
	})
	byName := map[string]*challenge.Challenge{}
	for _, c := range catalog {
		byName[c.Name] = c
	}
	cat := NewCatalog(catalog)
	for _, name := range []string{"One", "Two"} {
		p := cat.PlanIn(byName[name], nil)
		assert.True(t, p.Refused, name)
		assert.Contains(t, p.Reason, "shared:1 is also used by")
	}
	p := cat.PlanIn(byName["Base"], nil)
	assert.True(t, p.Refused, "builds an image another challenge builds FROM")
	assert.Equal(t, KindImage, cat.PlanIn(byName["App"], nil).Kind, "stage names and build args are not images")
	assert.Equal(t, KindImage, cat.PlanIn(byName["M1"], nil).Kind, "challenges of one folder share their image")
	assert.True(t, cat.PlanIn(byName["App"], []string{"app"}).Refused, "protected image")
}

func TestBuild_EveryHostMustBuild(t *testing.T) {
	c := load(t, map[string]string{"challenge.yml": httpChall, "Dockerfile": "FROM x\n"})
	runner := &fakeRunner{respond: func(map[string]interface{}) ([]ansible.HostResult, error) {
		return []ansible.HostResult{
			{Host: "h1", Task: "Create build directory"}, {Host: "h1", Task: "Build"},
			{Host: "h2", Task: "Create build directory"}, // h2 never ran the build
		}, nil
	}}
	log := &memLog{}
	res := (&Builder{Runner: runner}).Build(context.Background(), confWith(1), []*challenge.Challenge{c}, nil, false, log)
	assert.Equal(t, []*challenge.Challenge{c}, res.Failed)
	assert.Contains(t, log.text(), "did not run on h2")

	runner.respond = func(map[string]interface{}) ([]ansible.HostResult, error) { return nil, nil }
	res = (&Builder{Runner: runner}).Build(context.Background(), confWith(1), []*challenge.Challenge{c}, nil, false, log)
	assert.Equal(t, []*challenge.Challenge{c}, res.Failed, "no host at all")
}
