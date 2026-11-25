package build

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/internal/pathutil"
	"github.com/28Pollux28/galvanize/pkg/config"
	yaml "github.com/oasdiff/yaml3"
)

// What a challenge repository may build. Its content comes from whoever can
// push to it, and a build runs on every Ansible host, so:
//   - names (images, services, Dockerfiles) must be plain names, never
//     options or template expressions;
//   - a compose build section may only use keys that read the build context
//     (no host paths, host network, secrets, SSH agent or extra tags);
//   - a challenge may not tag an image that another challenge folder uses
//     (deploys, builds, or builds FROM), nor one of the platform's images:
//     the deployments run with pull: missing, so a local tag of that name
//     would replace the real image for everyone using it.

const label = `[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?`

var (
	// Docker image reference: [domain[:port]/]path[:tag][@digest]
	// (the first component is a registry only with a dot, a port or as
	// localhost; otherwise it is a lowercase path component)
	imageRefRe = regexp.MustCompile(`^(?:(?:localhost|` + label + `(?:\.` + label + `)+)(?::[0-9]+)?/|` + label + `:[0-9]+/)?` +
		`[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*` +
		`(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(?:@sha256:[a-f0-9]{64})?$`)
	serviceRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	fileRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	fromRe    = regexp.MustCompile(`(?i)^\s*FROM\s+(?:--\S+\s+)*(\S+)(?:\s+AS\s+(\S+))?`)
)

// DefaultProtectedImages are used when build.protected_images is not set:
// Galvanize's own images and the services it runs with, and the usual
// databases. The platform in front of Galvanize lists its own images in
// build.protected_images.
var DefaultProtectedImages = []string{
	"traefik", "mariadb", "mysql", "redis", "postgres",
	"ghcr.io/28pollux28/",
}

// ValidImageRef reports whether s is a plain image reference.
func ValidImageRef(s string) bool { return len(s) <= 255 && imageRefRe.MatchString(s) }

// NormalizeImage returns the canonical form of an image reference: without
// the default registry and library namespace, with ":latest" when it has no
// tag or digest ("nginx" and "docker.io/library/nginx:latest" are the same).
func NormalizeImage(ref string) string {
	ref = strings.TrimSpace(ref)
	for _, p := range []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"} {
		if strings.HasPrefix(strings.ToLower(ref), p) {
			ref = ref[len(p):]
			break
		}
	}
	ref = strings.TrimPrefix(ref, "library/")
	last := ref[strings.LastIndex(ref, "/")+1:]
	if !strings.Contains(last, ":") && !strings.Contains(ref, "@") {
		ref += ":latest"
	}
	return ref
}

// repository is a normalized reference without its tag and digest.
func repository(normalized string) string {
	if i := strings.Index(normalized, "@"); i >= 0 {
		normalized = normalized[:i]
	}
	if i := strings.LastIndex(normalized, ":"); i > strings.LastIndex(normalized, "/") {
		normalized = normalized[:i]
	}
	return normalized
}

// Protected reports whether image matches an entry of protected: an image
// repository (any tag), or a prefix when the entry ends with "/".
func Protected(image string, protected []string) bool {
	repo := repository(NormalizeImage(image))
	for _, p := range protected {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(repo, strings.TrimPrefix(p, "docker.io/")) {
				return true
			}
			continue
		}
		if repo == repository(NormalizeImage(p)) {
			return true
		}
	}
	return false
}

// ProtectedImages returns build.protected_images, or the defaults.
func ProtectedImages(conf *config.Config) []string {
	if conf != nil && conf.Instancer.Build.ProtectedImages != nil {
		return conf.Instancer.Build.ProtectedImages
	}
	return DefaultProtectedImages
}

// ImageUsers maps each image (normalized) used by the catalog to the
// challenge folders using it.
type ImageUsers map[string]map[string]bool

// Catalog is what planning builds needs from every challenge definition:
// each one's plan, and the images each challenge folder deploys, builds and
// builds from. Making it reads every compose file and Dockerfile, so it is
// made once per challenge index (see NewCatalog), and never changed.
type Catalog struct {
	challenges []*challenge.Challenge
	plans      map[*challenge.Challenge]Plan
	users      ImageUsers
}

// NewCatalog plans every challenge of catalog and indexes their images.
func NewCatalog(catalog []*challenge.Challenge) *Catalog {
	cat := &Catalog{
		challenges: append([]*challenge.Challenge(nil), catalog...),
		plans:      make(map[*challenge.Challenge]Plan, len(catalog)),
		users:      ImageUsers{},
	}
	add := func(image, dir string) {
		if strings.TrimSpace(image) == "" {
			return
		}
		n := NormalizeImage(image)
		if cat.users[n] == nil {
			cat.users[n] = map[string]bool{}
		}
		cat.users[n][dir] = true
	}
	seenDirs := map[string]bool{}
	for _, c := range catalog {
		dir := filepath.Clean(c.Dir)
		plan := PlanFor(c)
		cat.plans[c] = plan
		for _, img := range plan.Images {
			add(img, dir)
		}
		if image, ok := c.DeployParameters["image"].(string); ok {
			add(image, dir)
		}
		if def, ok := c.DeployParameters["compose_definition"].(string); ok {
			for _, img := range composeImages(def) {
				add(img, dir)
			}
		}
		if !seenDirs[dir] {
			seenDirs[dir] = true
			for _, img := range baseImages(dir) {
				add(img, dir)
			}
		}
	}
	return cat
}

// Of reports whether cat was made from these challenges: the index makes
// new Challenge values each time it is rebuilt.
func (cat *Catalog) Of(catalog []*challenge.Challenge) bool {
	if cat == nil || len(cat.challenges) != len(catalog) {
		return false
	}
	for i, c := range catalog {
		if cat.challenges[i] != c {
			return false
		}
	}
	return true
}

// Plan is PlanFor(c), computed once for the challenges of the catalog.
func (cat *Catalog) Plan(c *challenge.Challenge) Plan {
	if cat != nil {
		if plan, ok := cat.plans[c]; ok {
			return plan
		}
	}
	return PlanFor(c)
}

// PlanIn is Plan, refused (KindNone, Refused set) when the challenge may not
// build its images: see checkOwnership.
func (cat *Catalog) PlanIn(c *challenge.Challenge, protected []string) Plan {
	plan := cat.Plan(c)
	if plan.Kind == KindNone {
		return plan
	}
	var users ImageUsers
	if cat != nil {
		users = cat.users
	}
	if problem := checkOwnership(c, plan, users, protected); problem != "" {
		return Plan{Kind: KindNone, Refused: true, Reason: "refused: " + problem, Images: plan.Images}
	}
	return plan
}

// composeImages lists the images of a compose document's services.
func composeImages(def string) []string {
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(def), &doc); err != nil {
		return nil
	}
	services, _ := doc["services"].(map[string]interface{})
	return pulledImages(services)
}

// baseImages lists the FROM images of the Dockerfiles in dir (stages of the
// same Dockerfile and references built from build arguments left out).
func baseImages(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isDockerfile(d.Name()) || !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()
		stages := map[string]bool{}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			m := fromRe.FindStringSubmatch(scanner.Text())
			if m == nil {
				continue
			}
			ref := m[1]
			if !stages[strings.ToLower(ref)] && !strings.Contains(ref, "$") && strings.ToLower(ref) != "scratch" {
				out = append(out, ref)
			}
			if m[2] != "" {
				stages[strings.ToLower(m[2])] = true
			}
		}
		return nil
	})
	return out
}

// checkOwnership returns why c may not build plan's images, or "".
func checkOwnership(c *challenge.Challenge, plan Plan, users ImageUsers, protected []string) string {
	dir := filepath.Clean(c.Dir)
	var problems []string
	for _, img := range plan.Images {
		if Protected(img, protected) {
			problems = append(problems, fmt.Sprintf("%s is a platform image (build.protected_images)", img))
			continue
		}
		var others []string
		for d := range users[NormalizeImage(img)] {
			if d != dir {
				others = append(others, d)
			}
		}
		if len(others) > 0 {
			sort.Strings(others)
			problems = append(problems, fmt.Sprintf("%s is also used by %s", img, strings.Join(others, ", ")))
		}
	}
	return strings.Join(problems, "; ")
}

// buildKeys are the compose build keys a challenge may use. Left out on
// purpose: network (host network), additional_contexts, secrets, ssh,
// cache_from/cache_to (host paths), extra_hosts, tags (more image names),
// entitlements, privileged, isolation.
var buildKeys = map[string]bool{
	"context": true, "dockerfile": true, "dockerfile_inline": true, "args": true,
	"target": true, "labels": true, "shm_size": true, "no_cache": true, "pull": true,
	"platforms": true,
}

// sanitizeBuild checks a compose build section. Contexts and Dockerfiles must
// be relative paths inside the challenge folder (composeDir being the folder
// of the compose file, which the paths are relative to).
func sanitizeBuild(raw interface{}, challDir, composeDir string) (map[string]interface{}, error) {
	var b map[string]interface{}
	switch v := raw.(type) {
	case string:
		b = map[string]interface{}{"context": v}
	case map[string]interface{}:
		b = map[string]interface{}{}
		for k, val := range v {
			if !buildKeys[k] {
				return nil, fmt.Errorf("build.%s is not allowed", k)
			}
			b[k] = val
		}
	case nil:
		b = map[string]interface{}{}
	default:
		return nil, fmt.Errorf("build must be a path or a mapping")
	}
	ctx := "."
	if v, ok := b["context"]; ok {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("build.context must be a path")
		}
		ctx = s
	}
	ctxDir, err := insideDir(challDir, composeDir, ctx, "build.context")
	if err != nil {
		return nil, err
	}
	if v, ok := b["dockerfile"]; ok {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("build.dockerfile must be a path")
		}
		if _, err := insideDir(challDir, ctxDir, s, "build.dockerfile"); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// insideDir resolves rel against from and checks it stays inside root.
func insideDir(root, from, rel, what string) (string, error) {
	lower := strings.ToLower(rel)
	if strings.Contains(rel, "://") || strings.HasPrefix(lower, "git@") || strings.HasPrefix(lower, "github.com/") {
		return "", fmt.Errorf("%s %q: remote contexts are not allowed", what, rel)
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "~") || strings.Contains(rel, "\\") {
		return "", fmt.Errorf("%s %q must be a relative path", what, rel)
	}
	p := filepath.Join(from, filepath.FromSlash(rel))
	if !pathutil.Inside(root, p) {
		return "", fmt.Errorf("%s %q leaves the challenge folder", what, rel)
	}
	return p, nil
}
