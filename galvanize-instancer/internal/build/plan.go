package build

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/28Pollux28/galvanize/internal/challenge"
	yaml "github.com/oasdiff/yaml3"
)

// Kinds of build plan.
const (
	KindImage   = "image"   // docker build of one Dockerfile
	KindCompose = "compose" // docker compose build of the services with a build section
	KindNone    = "none"    // nothing to build: images are pulled
)

// buildComposeName is the Compose file Galvanize writes next to the
// challenge's own one in the build archive: only the services to build, with
// their build section and image name.
const buildComposeName = ".galvanize-build.compose.yaml"

// Plan says what building a challenge means.
type Plan struct {
	Kind   string
	Reason string   // why there is nothing to build (KindNone)
	Images []string // images the build produces
	// Refused: there is something to build, but the challenge may not build
	// it (see policy.go); Reason says why. Kind is KindNone.
	Refused bool
	// KindImage
	Dockerfile string
	// KindCompose
	Services   []string
	composeDir string // folder of the compose file, inside the challenge folder
	composeDoc []byte // the generated build compose file
}

// Label names a challenge in logs.
func Label(c *challenge.Challenge) string { return c.Category + "/" + c.Name }

// PlanFor works out how to build c.
func PlanFor(c *challenge.Challenge) Plan {
	if c.PlaybookName == "custom_compose" {
		return planCompose(c)
	}
	image, _ := c.DeployParameters["image"].(string)
	image = strings.TrimSpace(image)
	if image == "" {
		return Plan{Kind: KindNone, Reason: "no deploy_parameters.image"}
	}
	if !ValidImageRef(image) {
		return refused("deploy_parameters.image %q is not a valid image name", image)
	}
	dockerfile := findDockerfile(c.Dir)
	if dockerfile == "" {
		return Plan{Kind: KindNone, Reason: "no Dockerfile in the challenge folder: the image is pulled", Images: []string{image}}
	}
	if !fileRe.MatchString(dockerfile) {
		return refused("Dockerfile name %q: use letters, digits, '.', '_' and '-' only", dockerfile)
	}
	return Plan{Kind: KindImage, Images: []string{image}, Dockerfile: dockerfile}
}

func refused(format string, args ...interface{}) Plan {
	return Plan{Kind: KindNone, Refused: true, Reason: "refused: " + fmt.Sprintf(format, args...)}
}

// isDockerfile reports whether a file name is a Dockerfile (Dockerfile,
// dockerfile, Dockerfile.prod...).
func isDockerfile(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "dockerfile")
}

func findDockerfile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && isDockerfile(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	// Prefer the plain name when several Dockerfiles exist
	for _, n := range names {
		if n == "Dockerfile" || n == "dockerfile" {
			return n
		}
	}
	return names[0]
}

// composeSource returns the compose document to build from, and the folder
// its relative build contexts are relative to. The challenge's Compose file
// (found and checked by the index) wins over an inline definition: inline
// definitions usually reference prebuilt images, while the file next to them
// has the build sections.
func composeSource(c *challenge.Challenge) ([]byte, string, error) {
	if c.ComposeFile != "" {
		data, err := os.ReadFile(c.ComposeFile)
		if err != nil {
			return nil, "", err
		}
		return data, filepath.Dir(c.ComposeFile), nil
	}
	switch def := c.DeployParameters["compose_definition"].(type) {
	case string:
		return []byte(def), c.Dir, nil
	case map[string]interface{}:
		data, err := yaml.Marshal(def)
		return data, c.Dir, err
	}
	return nil, "", fmt.Errorf("no compose definition")
}

func planCompose(c *challenge.Challenge) Plan {
	data, dir, err := composeSource(c)
	if err != nil {
		return Plan{Kind: KindNone, Reason: err.Error()}
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Plan{Kind: KindNone, Reason: fmt.Sprintf("invalid compose definition: %v", err)}
	}
	services, _ := doc["services"].(map[string]interface{})
	buildServices := map[string]interface{}{}
	plan := Plan{Kind: KindCompose, composeDir: dir}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		svc, ok := services[name].(map[string]interface{})
		if !ok {
			continue
		}
		raw, hasBuild := svc["build"]
		if !hasBuild {
			continue
		}
		if !serviceRe.MatchString(name) {
			return refused("service name %q: use letters, digits, '.', '_' and '-' only", name)
		}
		b, err := sanitizeBuild(raw, c.Dir, dir)
		if err != nil {
			return refused("service %s: %v", name, err)
		}
		image, _ := svc["image"].(string)
		image = strings.TrimSpace(image)
		if image == "" {
			image = challenge.BuiltImageTag(c.Category, c.Name, name)
		}
		if !ValidImageRef(image) {
			return refused("service %s: %q is not a valid image name", name, image)
		}
		if deployed, ok := deployedImage(c, name); ok && NormalizeImage(deployed) != NormalizeImage(image) {
			return refused("service %s: %s builds %s but the deployment (deploy_parameters.compose_definition) runs %s",
				name, filepath.Base(c.ComposeFile), image, deployed)
		}
		buildServices[name] = map[string]interface{}{"build": b, "image": image}
		plan.Services = append(plan.Services, name)
		plan.Images = append(plan.Images, image)
	}
	if len(plan.Services) == 0 {
		return Plan{Kind: KindNone, Reason: "no compose service has a build section: images are pulled", Images: pulledImages(services)}
	}
	out, err := yaml.Marshal(map[string]interface{}{"services": buildServices})
	if err != nil {
		return Plan{Kind: KindNone, Reason: err.Error()}
	}
	plan.composeDoc = out
	return plan
}

// deployedImage returns the image service runs in the deployment when the
// build reads another compose document than the deployment does (the
// Compose file next to an inline compose_definition).
func deployedImage(c *challenge.Challenge, service string) (string, bool) {
	if c.ComposeFile == "" || !c.ComposeInline {
		return "", false
	}
	def, ok := c.DeployParameters["compose_definition"].(string)
	if !ok {
		return "", false
	}
	var doc map[string]interface{}
	if yaml.Unmarshal([]byte(def), &doc) != nil {
		return "", false
	}
	services, _ := doc["services"].(map[string]interface{})
	svc, ok := services[service].(map[string]interface{})
	if !ok {
		return "", false
	}
	image, _ := svc["image"].(string)
	return strings.TrimSpace(image), true
}

func pulledImages(services map[string]interface{}) []string {
	var images []string
	for _, raw := range services {
		if svc, ok := raw.(map[string]interface{}); ok {
			if image, _ := svc["image"].(string); image != "" && ValidImageRef(strings.TrimSpace(image)) {
				images = append(images, image)
			}
		}
	}
	sort.Strings(images)
	return images
}
