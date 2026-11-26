package build

import (
	"context"
	"fmt"
	"strings"

	"github.com/28Pollux28/galvanize/internal/ansible"
	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/internal/operation"
	"github.com/28Pollux28/galvanize/pkg/config"
)

// Image actions.
const (
	ImagesStatus = "status"
	ImagesRemove = "remove"
)

const statusScript = `for i in "$@"; do if docker image inspect "$i" >/dev/null 2>&1; then echo "present $i"; else echo "absent $i"; fi; done`
const removeScript = `for i in "$@"; do if ! docker image inspect "$i" >/dev/null 2>&1; then echo "absent $i"; elif docker image rm "$i" >/dev/null 2>&1; then echo "removed $i"; else echo "failed $i"; fi; done`

// States of an image on a host the script could not run on: the host did
// not answer, or the task failed there (e.g. no Docker).
const (
	StateUnreachable = "unreachable"
	StateError       = "error"
)

// ImagesResult is the state of the images of an Images run.
type ImagesResult struct {
	// States maps each image to its state on each host: present or absent
	// (status), removed, absent or failed (remove: the image is still
	// there), or StateUnreachable / StateError for every image on a host
	// the script did not run on.
	States map[string]map[string]string
	// Challenges maps each challenge to the images checked or removed.
	Challenges map[*challenge.Challenge][]string
}

// Images checks (status) or removes (remove) the challenges' images on
// every host. Only images the challenge may build are removed (see
// Catalog.PlanIn): never a pulled image, one another challenge uses, or a
// platform image.
func (b *Builder) Images(ctx context.Context, conf *config.Config, challs []*challenge.Challenge, cat *Catalog, action string, log operation.Logger) (*ImagesResult, error) {
	script := statusScript
	if action == ImagesRemove {
		script = removeScript
	} else if action != ImagesStatus {
		return nil, fmt.Errorf("unknown image action %q", action)
	}
	out := &ImagesResult{States: map[string]map[string]string{}, Challenges: map[*challenge.Challenge][]string{}}
	var images []string
	seen := map[string]bool{}
	protected := ProtectedImages(conf)
	for _, c := range challs {
		plan := cat.Plan(c)
		if action == ImagesRemove {
			plan = cat.PlanIn(c, protected)
			if plan.Refused {
				log.Warnf("[%s] Images not removed, %s", Label(c), plan.Reason)
				continue
			}
			if plan.Kind == KindNone {
				// Pulled images are not Galvanize's to remove
				continue
			}
		}
		for _, img := range plan.Images {
			out.Challenges[c] = append(out.Challenges[c], img)
			if !seen[img] {
				seen[img] = true
				images = append(images, img)
			}
		}
	}
	if len(images) == 0 {
		log.Infof("No image to %s", map[string]string{ImagesStatus: "check", ImagesRemove: "remove"}[action])
		return out, nil
	}
	results, err := b.Runner.Run(ctx, conf, ansible.PlaybookImages, map[string]interface{}{
		"images_script": script,
		"images":        images,
	}, 0)
	set := func(image, host, state string) {
		if out.States[image] == nil {
			out.States[image] = map[string]string{}
		}
		out.States[image][host] = state
	}
	for _, r := range results {
		if r.Failed || r.Unreachable {
			state := StateError
			if r.Unreachable {
				state = StateUnreachable
			}
			log.Errorf("%s failed on %s (%s): %s", r.Task, r.Host, state, r.Msg)
			for _, img := range images {
				set(img, r.Host, state)
			}
			continue
		}
		for _, line := range r.Stdout {
			state, image, found := strings.Cut(strings.TrimSpace(line), " ")
			if !found {
				continue
			}
			set(image, r.Host, state)
			switch state {
			case "removed":
				log.Successf("Removed %s on %s", image, r.Host)
			case "failed":
				log.Warnf("Could not remove %s on %s (used by a container?)", image, r.Host)
			case "absent":
				log.Debugf("%s is not on %s", image, r.Host)
			case "present":
				log.Debugf("%s is on %s", image, r.Host)
			}
		}
	}
	if err != nil && len(results) == 0 {
		return nil, err
	}
	return out, nil
}
