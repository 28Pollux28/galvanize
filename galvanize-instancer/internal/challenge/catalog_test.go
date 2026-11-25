package challenge

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalog_ListsEveryTypeWithTheRawDefinition(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, "repo/misc/intro", "name: Intro\ncategory: Misc\ntype: standard\nflags: [\"F{x}\"]\nhints:\n  - {content: h, cost: 5}\n")
	writeChallenge(t, dir, "repo/web/app", "name: App\ncategory: Web\ntype: zync\ndeploy_parameters:\n  image: app\n")

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	assert.Len(t, idx.GetAll(), 1, "only zync challenges are deployable")
	cat := idx.Catalog()
	require.Len(t, cat, 2)
	assert.Equal(t, "Intro", cat[0].Name, "sorted by path")
	assert.Equal(t, []interface{}{"F{x}"}, cat[0].Definition["flags"])
	hints := cat[0].Definition["hints"].([]interface{})
	assert.Equal(t, 5, hints[0].(map[string]interface{})["cost"])
	assert.Equal(t, filepath.Join(dir, "repo/misc/intro/challenge.yml"), cat[0].File)
}

func TestCatalog_DefinitionIsNotChangedByDeploymentDefaults(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, "repo/web/app", "name: App\ncategory: Web\ntype: zync\nplaybook_name: tcp\ndeploy_parameters:\n  image: app\n  port: 22\n")
	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	c := idx.Catalog()[0]
	assert.Contains(t, c.DeployParameters, "published_ports", "deployment defaults applied")
	assert.NotContains(t, c.Definition["deploy_parameters"], "published_ports", "raw definition untouched")
}

func TestBuildIndex_SkipsHiddenFolders(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, ".galvanize-tmp/x/web/app", "name: Tmp\ncategory: Web\ntype: zync\n")
	writeChallenge(t, dir, "repo/.github/app", "name: Gh\ncategory: Web\ntype: zync\n")
	writeChallenge(t, dir, "repo/web/app", "name: App\ncategory: Web\ntype: zync\n")
	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	require.Len(t, idx.Catalog(), 1)
	assert.Equal(t, "App", idx.Catalog()[0].Name)
}

func TestBuildIndex_TagsBuiltServicesWithoutImage(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, "repo/web/lab", "name: My Lab\ncategory: Web\ntype: zync\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "repo/web/lab/compose.yml"),
		[]byte("services:\n  web:\n    build: .\n  db:\n    image: mariadb:11\n  api:\n    build: ./api\n    image: lab-api:2\n"), 0o644))
	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	c, err := idx.Get("Web", "My Lab")
	require.NoError(t, err)
	def := c.DeployParameters["compose_definition"].(string)
	assert.Contains(t, def, "image: "+BuiltImageTag("Web", "My Lab", "web"))
	assert.Contains(t, def, "image: lab-api:2", "explicit image kept")
	assert.Contains(t, def, "image: mariadb:11")
	assert.Equal(t, filepath.Join(dir, "repo/web/lab/compose.yml"), c.ComposeFile)
}

func TestImageSlug(t *testing.T) {
	assert.Equal(t, "la-fondatrice-1", ImageSlug("La Fondatrice 1"))
	assert.Equal(t, "rabb-it-h-o-l", ImageSlug("Rabb-IT H.O.L"))
	assert.Regexp(t, `^galvanize/web-a-b-svc-[0-9a-f]{10}:latest$`, BuiltImageTag("Web", "A  b!", "svc"))
	// Names sharing a slug get different tags
	assert.NotEqual(t, BuiltImageTag("web", "a-b", "svc"), BuiltImageTag("web-a", "b", "svc"))
	assert.NotEqual(t, BuiltImageTag("Web", "app", "svc"), BuiltImageTag("web", "app", "svc"))
	assert.NotEqual(t, BuiltImageTag("Web", "défi", "svc"), BuiltImageTag("Web", "défo", "svc"))
	assert.Regexp(t, `^galvanize/x-x-x-[0-9a-f]{10}:latest$`, BuiltImageTag("密码", "挑战", "服务"))
}

func TestBuildIndex_ComposeFileStaysInTheChallengeFolder(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "host-compose.yml"), []byte("services:\n  x:\n    image: a\n"), 0o644))
	writeChallenge(t, dir, "repo/web/a", "name: A\ncategory: Web\ntype: zync\ndeploy_parameters:\n  compose_file: ../../../host-compose.yml\n")
	writeChallenge(t, dir, "repo/web/b", "name: B\ncategory: Web\ntype: zync\ndeploy_parameters:\n  compose_file: /etc/hosts\n")
	require.NoError(t, os.Symlink(filepath.Join(dir, "host-compose.yml"), filepath.Join(dir, "repo/web/c-link.yml")))
	writeChallenge(t, dir, "repo/web/c", "name: C\ncategory: Web\ntype: zync\ndeploy_parameters:\n  compose_file: ../c-link.yml\n")
	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	assert.Empty(t, idx.GetAll())
	require.Len(t, idx.Skipped(), 3)
	for _, sk := range idx.Skipped() {
		assert.Contains(t, sk.Reason, "inside the challenge folder", sk.Path)
	}
}

func TestBuildIndex_UntaggedJinjaComposeIsKept(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, "repo/web/j", "name: J\ncategory: Web\ntype: zync\nplaybook_name: custom_compose\ndeploy_parameters:\n  compose_definition: |\n    services:\n      web:\n        image: {{ image }}\n")
	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	c, err := idx.Get("Web", "J")
	require.NoError(t, err)
	assert.Contains(t, c.DeployParameters["compose_definition"], "{{ image }}", "deployed as written")
}

func TestBuildIndex_ComposeFileNextToAnInlineDefinition(t *testing.T) {
	dir := t.TempDir()
	inline := "name: %s\ncategory: Web\ntype: zync\nplaybook_name: custom_compose\ndeploy_parameters:\n" +
		"  compose_definition: |\n    services:\n      web:\n        image: w\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "host-compose.yml"), []byte("services:\n  x:\n    build: .\n"), 0o644))
	// Explicit compose_file: checked even though the inline definition is deployed
	writeChallenge(t, dir, "repo/web/a", fmt.Sprintf(inline, "A")+"  compose_file: ../../../host-compose.yml\n")
	writeChallenge(t, dir, "repo/web/b", fmt.Sprintf(inline, "B")+"  compose_file: build/compose.yml\n")
	writeFile(t, dir, "repo/web/b/build", "compose.yml", "services:\n  web:\n    build: .\n    image: w\n")
	// A standard name, as a symlink leading outside
	writeChallenge(t, dir, "repo/web/c", fmt.Sprintf(inline, "C"))
	require.NoError(t, os.Symlink(filepath.Join(dir, "host-compose.yml"), filepath.Join(dir, "repo/web/c/compose.yml")))

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	require.Len(t, idx.Skipped(), 2)
	for _, sk := range idx.Skipped() {
		assert.Contains(t, sk.Reason, "inside the challenge folder", sk.Path)
	}
	b, err := idx.Get("Web", "B")
	require.NoError(t, err)
	assert.True(t, b.ComposeInline)
	assert.Equal(t, filepath.Join(dir, "repo/web/b/build/compose.yml"), b.ComposeFile, "read to build")
	assert.Contains(t, b.DeployParameters["compose_definition"], "image: w", "the inline definition is deployed")
	assert.NotContains(t, b.DeployParameters, "compose_file")
}

func TestFiles_LookedUpInTheFolderThenFiles(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, "r/misc/a", "name: A\ncategory: Misc\ntype: standard\nfiles: [x.txt, ./files/y.txt, 3, \"\"]\n")
	writeFile(t, dir, "r/misc/a/files", "x.txt", "x")
	writeFile(t, dir, "r/misc/a/files", "y.txt", "yy")
	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	files := idx.Catalog()[0].Files()
	require.Len(t, files, 3)
	assert.Equal(t, File{Declared: "x.txt", Path: filepath.Join(dir, "r/misc/a/files/x.txt"), Size: 1}, files[0])
	assert.Equal(t, filepath.Join(dir, "r/misc/a/files/y.txt"), files[1].Path)
	assert.Equal(t, File{Declared: "3", Error: "not a file path"}, files[2])
}
