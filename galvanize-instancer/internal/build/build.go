// Package build builds challenge images where they run: on every Ansible
// host, with that host's Docker, so that deployments find them locally (the
// playbooks deploy with pull: missing).
//
// A challenge deployed from one image (http, tcp...) is built from the
// Dockerfile of its folder, tagged deploy_parameters.image. A compose
// challenge (custom_compose) has the services with a build section built,
// each tagged with its image, or challenge.BuiltImageTag when it has none
// (the same name the index gives it for deployments).
//
// plan.go works out what to build, policy.go what a challenge may build,
// archive.go what is sent to the hosts, images.go checks and removes images.
package build

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/28Pollux28/galvanize/internal/ansible"
	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/internal/operation"
	"github.com/28Pollux28/galvanize/pkg/config"
	"go.uber.org/zap"
)

// Builder runs builds and image maintenance on the Ansible hosts.
type Builder struct {
	Runner ansible.UtilityRunner
}

// failedLines is how much of a failed build's output is logged.
const failedLines = 60

// Result is what a Build did with each challenge, every list sorted by
// definition file.
type Result struct {
	Built   []*challenge.Challenge
	Failed  []*challenge.Challenge // refused builds included
	Skipped []*challenge.Challenge // nothing to build
}

// Build builds the challenges, up to build.concurrency at a time. cat is the
// whole catalog, to check which images each challenge may build.
func (b *Builder) Build(ctx context.Context, conf *config.Config, challs []*challenge.Challenge, cat *Catalog, noCache bool, log operation.Logger) Result {
	var mu sync.Mutex
	res := Result{Built: []*challenge.Challenge{}, Failed: []*challenge.Challenge{}, Skipped: []*challenge.Challenge{}}
	sem := make(chan struct{}, conf.Instancer.Build.ConcurrencyOrDefault())
	var wg sync.WaitGroup
	protected := ProtectedImages(conf)
	for _, c := range challs {
		plan := cat.PlanIn(c, protected)
		if plan.Refused {
			log.Errorf("[%s] Not built, %s", Label(c), plan.Reason)
			res.Failed = append(res.Failed, c)
			continue
		}
		if plan.Kind == KindNone {
			log.Debugf("[%s] Nothing to build: %s", Label(c), plan.Reason)
			res.Skipped = append(res.Skipped, c)
			continue
		}
		wg.Add(1)
		go func(c *challenge.Challenge, plan Plan) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				res.Failed = append(res.Failed, c)
				mu.Unlock()
				return
			}
			defer func() { <-sem }()
			ok := b.safeBuildOne(ctx, conf, c, plan, noCache, log)
			mu.Lock()
			if ok {
				res.Built = append(res.Built, c)
			} else {
				res.Failed = append(res.Failed, c)
			}
			mu.Unlock()
		}(c, plan)
	}
	wg.Wait()
	for _, list := range [][]*challenge.Challenge{res.Built, res.Failed, res.Skipped} {
		sort.Slice(list, func(i, j int) bool { return list[i].File < list[j].File })
	}
	return res
}

// safeBuildOne turns a panic of buildOne into a failed build: it runs in a
// goroutine of its own, out of reach of the operation's safeCall.
func (b *Builder) safeBuildOne(ctx context.Context, conf *config.Config, c *challenge.Challenge, plan Plan, noCache bool, log operation.Logger) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			zap.S().Errorf("Build of %s panicked: %v", Label(c), r)
			log.Errorf("[%s] Build failed: internal error: %v", Label(c), r)
			ok = false
		}
	}()
	return b.buildOne(ctx, conf, c, plan, noCache, log)
}

func (b *Builder) buildOne(ctx context.Context, conf *config.Config, c *challenge.Challenge, plan Plan, noCache bool, log operation.Logger) bool {
	key := Label(c)
	archive, workdir, err := writeArchive(c, plan, log)
	if err != nil {
		log.Errorf("[%s] Cannot prepare the build: %v", key, err)
		return false
	}
	defer func() { _ = os.Remove(archive) }()

	argv := BuildArgv(c, plan, noCache)
	log.Infof("[%s] Building %s", key, strings.Join(plan.Images, ", "))
	ctx, cancel := context.WithTimeout(ctx, conf.Instancer.Build.TimeoutOrDefault())
	defer cancel()
	results, err := b.Runner.Run(ctx, conf, ansible.PlaybookBuild, map[string]interface{}{
		"build_archive": archive,
		"build_workdir": workdir,
		"build_argv":    argv,
	}, conf.Instancer.Build.TimeoutOrDefault())

	hosts, seen := map[string]bool{}, map[string]bool{}
	ok, hostFailed := err == nil, false
	for _, r := range results {
		seen[r.Host] = true
		if r.Failed || r.Unreachable {
			ok, hostFailed = false, true
			log.Errorf("[%s] %s failed on %s: %s", key, r.Task, r.Host, firstNonEmpty(r.Msg, "see the output below"))
			for _, line := range tail(append(append([]string{}, r.Stdout...), r.Stderr...), failedLines) {
				log.Infof("[%s]   %s", key, line)
			}
		} else if r.Task == "Build" && !r.Skipped {
			hosts[r.Host] = true
		}
	}
	// Without a failed host to explain it (a cancelled or timed out build
	// has no results), the error is the only explanation
	if err != nil && !hostFailed {
		log.Errorf("[%s] Build failed: %v", key, err)
	}
	if ok {
		// Every host must have run the build: a host without its result
		// would deploy an old image, or none
		for h := range seen {
			if !hosts[h] {
				log.Errorf("[%s] The build did not run on %s", key, h)
				ok = false
			}
		}
		if len(hosts) == 0 {
			log.Errorf("[%s] The build ran on no host", key)
			ok = false
		}
	}
	if !ok {
		return false
	}
	names := make([]string, 0, len(hosts))
	for h := range hosts {
		names = append(names, h)
	}
	sort.Strings(names)
	log.Successf("[%s] Built %s on %s", key, strings.Join(plan.Images, ", "), strings.Join(names, ", "))
	return true
}

// BuildArgv is the build command run in the challenge folder on each host.
func BuildArgv(c *challenge.Challenge, plan Plan, noCache bool) []string {
	if plan.Kind == KindImage {
		argv := []string{"docker", "build", "--progress=plain"}
		if noCache {
			argv = append(argv, "--no-cache")
		}
		return append(argv, "-t", plan.Images[0], "-f", plan.Dockerfile, ".")
	}
	project := "galvanize-build-" + challenge.BuildSlug(c.Category, c.Name)
	argv := []string{"docker", "compose", "--progress", "plain", "-f", buildComposeName, "-p", project, "build"}
	if noCache {
		argv = append(argv, "--no-cache")
	}
	return append(argv, plan.Services...)
}

func tail(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
