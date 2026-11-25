package pkg

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/28Pollux28/galvanize/internal/auth"
	"github.com/28Pollux28/galvanize/internal/build"
	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/internal/operation"
	"github.com/28Pollux28/galvanize/internal/pathutil"
	"github.com/28Pollux28/galvanize/internal/repo"
	"github.com/28Pollux28/galvanize/pkg/api"
	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/28Pollux28/galvanize/pkg/models"
	"github.com/28Pollux28/galvanize/pkg/utils"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// Repository, build and image management (admin API). Everything here works
// on challenge_dir and the Ansible hosts, without any knowledge of the CTF
// platform calling it.

// requireAdmin answers 401/403 and returns false unless the caller is admin
// with the manage scope (see auth.ScopeManage).
func requireAdmin(ctx echo.Context) (bool, error) {
	claims, err := auth.GetClaims(ctx)
	if err != nil {
		return false, ctx.JSON(401, api.Error{Message: utils.Ptr("Unauthorized")})
	}
	if claims.Role != "admin" {
		return false, ctx.JSON(403, api.Error{Message: utils.Ptr("Forbidden - Admin access required")})
	}
	if claims.Scope != auth.ScopeManage {
		return false, ctx.JSON(403, api.Error{Message: utils.Ptr("Forbidden - this endpoint needs an admin token with the \"manage\" scope")})
	}
	return true, nil
}

func apiError(ctx echo.Context, code int, format string, args ...interface{}) error {
	return ctx.JSON(code, api.Error{Message: utils.Ptr(fmt.Sprintf(format, args...))})
}

func (s *Server) repos() *repo.Manager {
	conf := s.confProv.GetConfig()
	return &repo.Manager{BaseDir: conf.Instancer.ChallengeDir, Config: conf.Instancer.Repositories, Git: s.gitBinary}
}

func toAPIOperation(snap operation.Snapshot) api.Operation {
	lines := make([]api.OperationLine, 0, len(snap.Lines))
	for _, l := range snap.Lines {
		lines = append(lines, api.OperationLine{Time: l.Time, Level: api.OperationLineLevel(l.Level), Message: l.Message})
	}
	op := api.Operation{
		Id: snap.ID, Kind: api.OperationKind(snap.Kind), Target: snap.Target,
		Status: api.OperationStatus(snap.Status), CreatedAt: snap.CreatedAt, FinishedAt: snap.FinishedAt,
		Lines: lines, LineCount: snap.LineCount,
	}
	if snap.Result != nil {
		op.Result = &snap.Result
	}
	return op
}

// startOperation starts fn and answers 202 with the operation, or 409.
func (s *Server) startOperation(ctx echo.Context, kind, target string, fn operation.Func) error {
	op, err := s.operations.Start(kind, target, fn)
	if errors.Is(err, operation.ErrBusy) {
		snap := op.Snapshot(0)
		return apiError(ctx, 409, "Operation %s (%s %s) is still running", snap.ID, snap.Kind, snap.Target)
	}
	if err != nil {
		return apiError(ctx, 500, "%v", err)
	}
	zap.S().Infof("Operation %s started: %s %s", op.ID, kind, target)
	return ctx.JSON(202, toAPIOperation(op.Snapshot(0)))
}

// reindex reloads the challenge index after challenge_dir changed.
func (s *Server) reindex(log operation.Logger) {
	conf := s.confProv.GetConfig()
	if err := s.challIdx.BuildIndex(conf.Instancer.ChallengeDir); err != nil {
		log.Errorf("Challenge index not reloaded: %v", err)
		return
	}
	updateChallengeIndexMetrics(s.challIdx)
	skipped := s.challIdx.Skipped()
	log.Infof("Challenge index reloaded: %d deployable challenge(s), %d definition(s) skipped", len(s.challIdx.GetAll()), len(skipped))
	for _, sk := range skipped {
		log.Warnf("Skipped %s: %s", s.relPath(sk.Path), sk.Reason)
	}
}

// relPath makes path relative to challenge_dir when it is inside it.
func (s *Server) relPath(path string) string {
	base := s.confProv.GetConfig().Instancer.ChallengeDir
	if rel, err := filepath.Rel(base, path); err == nil && pathutil.Inside(base, path) {
		return filepath.ToSlash(rel)
	}
	return path
}

// relPaths are the definition files of challs, relative to challenge_dir:
// how results name challenges.
func (s *Server) relPaths(challs []*challenge.Challenge) []string {
	out := make([]string, 0, len(challs))
	for _, c := range challs {
		out = append(out, s.relPath(c.File))
	}
	return out
}

// buildCatalog returns the build catalog of the current challenge index,
// made once per index: the index makes new Challenge values each time it is
// rebuilt. It is kept here rather than by the index, which the build
// package depends on.
func (s *Server) buildCatalog() *build.Catalog {
	catalog := s.challIdx.Catalog()
	s.buildCatMu.Lock()
	defer s.buildCatMu.Unlock()
	if !s.buildCat.Of(catalog) {
		s.buildCat = build.NewCatalog(catalog)
	}
	return s.buildCat
}

// liveDeployments returns the deployments that may still have containers.
func (s *Server) liveDeployments() ([]models.Deployment, error) {
	var deployments []models.Deployment
	err := s.db.Where("status IN ?", []string{
		models.DeploymentStatusStarting, models.DeploymentStatusRunning,
		models.DeploymentStatusStopping, models.DeploymentStatusError,
	}).Find(&deployments).Error
	return deployments, err
}

func deploymentOf(d models.Deployment, c *challenge.Challenge) bool {
	return strings.EqualFold(d.Category, c.Category) && (d.ChallengeName == c.InstanceKey() || d.ChallengeName == c.Name)
}

func describeDeployment(d models.Deployment) string {
	team := "unique"
	if d.TeamID != nil {
		team = "team " + *d.TeamID
	}
	return fmt.Sprintf("%s/%s (%s, %s)", d.Category, d.ChallengeName, team, d.Status)
}

// ── Repositories ─────────────────────────────────────────────────────────────

func (s *Server) ListRepositories(ctx echo.Context) error {
	if ok, err := requireAdmin(ctx); !ok {
		return err
	}
	mgr := s.repos()
	infos, err := mgr.List(ctx.Request().Context())
	if err != nil {
		return apiError(ctx, 500, "Cannot read the challenge folder: %v", err)
	}
	counts := map[string]int{}
	for _, c := range s.challIdx.Catalog() {
		counts[mgr.RepoOf(c.Dir)]++
	}
	out := api.RepositoryList{Repositories: make([]api.Repository, 0, len(infos)), CloneSources: []string{}}
	for _, info := range infos {
		r := api.Repository{Name: info.Name, IsGit: info.IsGit, Challenges: counts[info.Name]}
		if info.IsGit {
			r.Remote, r.Branch, r.Ref = utils.Ptr(info.Remote), utils.Ptr(info.Branch), utils.Ptr(info.Ref)
			r.Commit, r.CommitMessage, r.CommitTime = utils.Ptr(info.Commit), utils.Ptr(info.CommitMessage), info.CommitTime
			r.Dirty = utils.Ptr(info.Dirty)
		}
		if info.Error != "" {
			r.Error = utils.Ptr(info.Error)
		}
		out.Repositories = append(out.Repositories, r)
	}
	for _, src := range s.confProv.GetConfig().Instancer.Repositories.Sources {
		out.CloneSources = append(out.CloneSources, src.URLPrefix)
	}
	return ctx.JSON(200, out)
}

func (s *Server) CloneRepository(ctx echo.Context) error {
	if ok, err := requireAdmin(ctx); !ok {
		return err
	}
	var req api.CloneRepositoryRequest
	if err := ctx.Bind(&req); err != nil || strings.TrimSpace(req.Url) == "" {
		return apiError(ctx, 400, "Invalid request: url is required")
	}
	url := strings.TrimSpace(req.Url)
	name := strings.TrimSpace(deref(req.Name))
	if name == "" {
		name = repo.NameFromURL(url)
	}
	ref := strings.TrimSpace(deref(req.Ref))
	force := req.Force != nil && *req.Force
	mgr := s.repos()

	// Checks that need no git run answer synchronously
	if !repo.ValidName(name) {
		return apiError(ctx, 400, "Invalid repository name %q", name)
	}
	if len(mgr.Config.Sources) == 0 {
		return apiError(ctx, 403, "%v", repo.ErrCloneDisabled)
	}
	if err := config.CheckRepositoryURL(url); err != nil {
		return apiError(ctx, 403, "URL not allowed: %v", err)
	}
	if mgr.Config.SourceFor(url) == nil {
		return apiError(ctx, 403, "URL not allowed: it must be inside one of repositories.sources")
	}
	if _, err := mgr.Path(name); err == nil && !force {
		return apiError(ctx, 409, "Repository %s already exists (use force to replace it)", name)
	}
	if force {
		if blocked := s.deploymentsInRepo(mgr, name); len(blocked) > 0 {
			return apiError(ctx, 409, "Instances of %s are running, terminate them first: %s", name, strings.Join(blocked, ", "))
		}
	}

	return s.startOperation(ctx, "clone", name, func(opCtx context.Context, log operation.Logger) (map[string]interface{}, error) {
		created, err := mgr.Clone(opCtx, url, name, ref, force, log)
		if err != nil {
			return nil, err
		}
		info := mgr.Describe(opCtx, created)
		log.Successf("Cloned %s at %s (%s)", created, info.Commit, info.CommitMessage)
		s.reindex(log)
		return map[string]interface{}{"repository": created, "commit": info.Commit}, nil
	})
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// deploymentsInRepo lists the live deployments of the repository's challenges.
func (s *Server) deploymentsInRepo(mgr *repo.Manager, name string) []string {
	deployments, err := s.liveDeployments()
	if err != nil {
		return []string{fmt.Sprintf("(cannot check: %v)", err)}
	}
	var out []string
	for _, c := range s.challIdx.GetAll() {
		if mgr.RepoOf(c.Dir) != name {
			continue
		}
		for _, d := range deployments {
			if deploymentOf(d, c) {
				out = append(out, describeDeployment(d))
			}
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) PullRepository(ctx echo.Context, name string) error {
	if ok, err := requireAdmin(ctx); !ok {
		return err
	}
	mgr := s.repos()
	if _, err := mgr.Path(name); err != nil {
		return apiError(ctx, 404, "Repository not found: %s", name)
	}
	return s.startOperation(ctx, "pull", name, func(opCtx context.Context, log operation.Logger) (map[string]interface{}, error) {
		before, after, err := mgr.Pull(opCtx, name, log)
		if err != nil {
			return nil, err
		}
		if before == after {
			// Not "up to date": local changes may just have been discarded
			log.Infof("No new commit on %s (%s)", name, after)
		} else {
			log.Successf("%s updated: %s → %s", name, before, after)
		}
		s.reindex(log)
		s.warnOrphans(log)
		return map[string]interface{}{"before": before, "after": after}, nil
	})
}

// warnOrphans reports live deployments whose challenge left the index: they
// cannot be terminated until the challenge is back.
func (s *Server) warnOrphans(log operation.Logger) {
	deployments, err := s.liveDeployments()
	if err != nil {
		return
	}
	for _, d := range deployments {
		if _, err := s.challIdx.Get(d.Category, d.ChallengeName); err != nil {
			log.Warnf("Instance %s has no challenge any more: restore the challenge to terminate it", describeDeployment(d))
		}
	}
}

func (s *Server) RemoveRepository(ctx echo.Context, name string) error {
	if ok, err := requireAdmin(ctx); !ok {
		return err
	}
	mgr := s.repos()
	if _, err := mgr.Path(name); err != nil {
		return apiError(ctx, 404, "Repository not found: %s", name)
	}
	if blocked := s.deploymentsInRepo(mgr, name); len(blocked) > 0 {
		return apiError(ctx, 409, "Instances of %s's challenges exist, terminate them first: %s", name, strings.Join(blocked, ", "))
	}
	return s.startOperation(ctx, "remove", name, func(opCtx context.Context, log operation.Logger) (map[string]interface{}, error) {
		if err := mgr.Remove(name); err != nil {
			return nil, err
		}
		log.Successf("Removed %s", name)
		s.reindex(log)
		return map[string]interface{}{"repository": name}, nil
	})
}

// ── Challenge definitions and files ──────────────────────────────────────────

func toAPIPlan(p build.Plan, deployable bool) api.BuildPlan {
	out := api.BuildPlan{Kind: api.BuildPlanKind(p.Kind), Images: p.Images, Buildable: deployable && p.Kind != build.KindNone}
	if out.Images == nil {
		out.Images = []string{}
	}
	if p.Reason != "" {
		out.Reason = utils.Ptr(p.Reason)
	}
	if p.Refused {
		out.Refused = utils.Ptr(true)
	}
	return out
}

func (s *Server) toAPIFiles(files []challenge.File) []api.ChallengeFile {
	out := make([]api.ChallengeFile, 0, len(files))
	for _, f := range files {
		item := api.ChallengeFile{Declared: f.Declared}
		if f.Path != "" {
			item.Path, item.Size = utils.Ptr(s.relPath(f.Path)), utils.Ptr(f.Size)
		}
		if f.Error != "" {
			item.Error = utils.Ptr(f.Error)
		}
		out = append(out, item)
	}
	return out
}

func (s *Server) ListChallengeDefinitions(ctx echo.Context, params api.ListChallengeDefinitionsParams) error {
	if ok, err := requireAdmin(ctx); !ok {
		return err
	}
	mgr := s.repos()
	if params.Repo != nil && *params.Repo != "" {
		if _, err := mgr.Path(*params.Repo); err != nil {
			return apiError(ctx, 404, "Repository not found: %s", *params.Repo)
		}
	}
	out := api.ChallengeDefinitionList{Challenges: []api.ChallengeDefinition{}, Skipped: []api.SkippedChallenge{}}
	cat, protected := s.buildCatalog(), build.ProtectedImages(s.confProv.GetConfig())
	commits := map[string]string{}
	for _, c := range s.challIdx.Catalog() {
		repoName := mgr.RepoOf(c.Dir)
		if params.Repo != nil && *params.Repo != repoName {
			continue
		}
		deployable := false
		if c.Type == "zync" {
			if indexed, err := s.challIdx.Get(c.Category, c.Name); err == nil && indexed.File == c.File {
				deployable = true
			}
		}
		def := c.Definition
		if def == nil {
			def = map[string]interface{}{}
		}
		item := api.ChallengeDefinition{
			Category: c.Category, Name: c.Name, Type: c.Type, Repository: repoName,
			Path: s.relPath(c.File), Dir: s.relPath(c.Dir), Deployable: deployable,
			Build: toAPIPlan(cat.PlanIn(c, protected), deployable), Files: s.toAPIFiles(c.Files()), Definition: def,
		}
		if c.PlaybookName != "" {
			item.PlaybookName = utils.Ptr(c.PlaybookName)
		}
		if repoName != "" {
			commit, ok := commits[repoName]
			if !ok {
				commit = mgr.Commit(ctx.Request().Context(), repoName)
				commits[repoName] = commit
			}
			if commit != "" {
				item.Commit = utils.Ptr(commit)
			}
		}
		out.Challenges = append(out.Challenges, item)
	}
	for _, sk := range s.challIdx.Skipped() {
		rel := s.relPath(sk.Path)
		if params.Repo != nil && mgr.RepoOf(sk.Path) != *params.Repo {
			continue
		}
		out.Skipped = append(out.Skipped, api.SkippedChallenge{Path: rel, Reason: sk.Reason})
	}
	return ctx.JSON(200, out)
}

// ResolveChallengeFile returns the real path of a file inside the folder of
// a challenge of the catalog, rel being relative to challenge_dir. Paths
// through .git, or leading (symlinks included) outside the challenge folder,
// are refused. The int is the HTTP status for the error.
func (s *Server) ResolveChallengeFile(rel string) (string, int, error) {
	base := s.confProv.GetConfig().Instancer.ChallengeDir
	clean := filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", 400, fmt.Errorf("invalid path")
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".git" {
			return "", 400, fmt.Errorf("paths into .git are refused")
		}
	}
	requested := filepath.Join(base, clean)
	for _, c := range s.challIdx.Catalog() {
		if !pathutil.Inside(filepath.Clean(c.Dir), requested) {
			continue
		}
		real, err := pathutil.Resolve(c.Dir, requested)
		if errors.Is(err, pathutil.ErrOutside) {
			return "", 400, fmt.Errorf("path leads outside the challenge folder")
		}
		if err != nil {
			return "", 404, fmt.Errorf("file not found")
		}
		st, err := os.Stat(real)
		if err != nil || !st.Mode().IsRegular() {
			return "", 404, fmt.Errorf("file not found")
		}
		return real, 200, nil
	}
	return "", 404, fmt.Errorf("not a file of a challenge")
}

func (s *Server) GetChallengeFile(ctx echo.Context, params api.GetChallengeFileParams) error {
	if ok, err := requireAdmin(ctx); !ok {
		return err
	}
	real, code, err := s.ResolveChallengeFile(params.Path)
	if err != nil {
		return apiError(ctx, code, "%v", err)
	}
	if params.Checksum != nil && *params.Checksum {
		sum, size, err := sha1File(real)
		if err != nil {
			return apiError(ctx, 500, "Cannot read the file: %v", err)
		}
		return ctx.JSON(200, api.FileChecksum{Sha1: sum, Size: size})
	}
	return ctx.Attachment(real, filepath.Base(real))
}

func sha1File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha1.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ── Builds and images ────────────────────────────────────────────────────────

// selectChallenges returns the deployable challenges of repoName and/or refs
// (all of them when neither is given).
func (s *Server) selectChallenges(repoName *string, refs *[]api.ChallengeRef) ([]*challenge.Challenge, error) {
	mgr := s.repos()
	var out []*challenge.Challenge
	if refs != nil && len(*refs) > 0 {
		byPath := map[string]*challenge.Challenge{}
		for _, c := range s.challIdx.GetAll() {
			byPath[s.relPath(c.File)] = c
		}
		var missing []string
		for _, r := range *refs {
			var c *challenge.Challenge
			label := deref(r.Path)
			if label != "" {
				c = byPath[label]
			} else {
				label = deref(r.Category) + "/" + deref(r.Name)
				c, _ = s.challIdx.Get(deref(r.Category), deref(r.Name))
			}
			if c == nil || (repoName != nil && *repoName != "" && mgr.RepoOf(c.Dir) != *repoName) {
				missing = append(missing, label)
				continue
			}
			out = append(out, c)
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("not deployable challenges: %s", strings.Join(missing, ", "))
		}
	} else {
		if repoName != nil && *repoName != "" {
			if _, err := mgr.Path(*repoName); err != nil {
				return nil, fmt.Errorf("repository not found: %s", *repoName)
			}
		}
		for _, c := range s.challIdx.GetAll() {
			if repoName == nil || *repoName == "" || mgr.RepoOf(c.Dir) == *repoName {
				out = append(out, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

func target(repoName *string, challs []*challenge.Challenge) string {
	if len(challs) == 1 {
		return build.Label(challs[0])
	}
	if repoName != nil && *repoName != "" {
		return *repoName
	}
	return fmt.Sprintf("%d challenge(s)", len(challs))
}

func (s *Server) BuildChallenges(ctx echo.Context) error {
	if ok, err := requireAdmin(ctx); !ok {
		return err
	}
	var req api.BuildRequest
	if err := ctx.Bind(&req); err != nil {
		return apiError(ctx, 400, "Invalid request")
	}
	challs, err := s.selectChallenges(req.Repo, req.Challenges)
	if err != nil {
		return apiError(ctx, 400, "%v", err)
	}
	noCache := req.NoCache != nil && *req.NoCache
	conf := s.confProv.GetConfig()
	return s.startOperation(ctx, "build", target(req.Repo, challs), func(opCtx context.Context, log operation.Logger) (map[string]interface{}, error) {
		log.Infof("Building %d challenge(s) on %s, %d at a time", len(challs), conf.Instancer.Ansible.Inventory, conf.Instancer.Build.ConcurrencyOrDefault())
		res := s.builder.Build(opCtx, conf, challs, s.buildCatalog(), noCache, log)
		log.Infof("Built: %d, failed: %d, nothing to build: %d", len(res.Built), len(res.Failed), len(res.Skipped))
		result := map[string]interface{}{"built": s.relPaths(res.Built), "failed": s.relPaths(res.Failed), "skipped": s.relPaths(res.Skipped)}
		if len(res.Failed) > 0 && len(res.Built) == 0 {
			return result, fmt.Errorf("no challenge built")
		}
		return result, nil
	})
}

func (s *Server) ManageImages(ctx echo.Context) error {
	if ok, err := requireAdmin(ctx); !ok {
		return err
	}
	var req api.ImagesRequest
	if err := ctx.Bind(&req); err != nil {
		return apiError(ctx, 400, "Invalid request")
	}
	action := string(req.Action)
	if action != build.ImagesStatus && action != build.ImagesRemove {
		return apiError(ctx, 400, "action must be status or remove")
	}
	challs, err := s.selectChallenges(req.Repo, req.Challenges)
	if err != nil {
		return apiError(ctx, 400, "%v", err)
	}
	if action == build.ImagesRemove {
		if blocked := s.liveOf(challs); len(blocked) > 0 {
			return apiError(ctx, 409, "Instances use these images, terminate them first: %s", strings.Join(blocked, ", "))
		}
	}
	conf := s.confProv.GetConfig()
	return s.startOperation(ctx, "images", target(req.Repo, challs), func(opCtx context.Context, log operation.Logger) (map[string]interface{}, error) {
		res, err := s.builder.Images(opCtx, conf, challs, s.buildCatalog(), action, log)
		if err != nil {
			return nil, err
		}
		images, byPath := map[string]interface{}{}, map[string]interface{}{}
		for img, hosts := range res.States {
			images[img] = hosts
		}
		for c, imgs := range res.Challenges {
			byPath[s.relPath(c.File)] = imgs
		}
		return map[string]interface{}{"images": images, "challenges": byPath}, nil
	})
}

func (s *Server) liveOf(challs []*challenge.Challenge) []string {
	deployments, err := s.liveDeployments()
	if err != nil {
		return []string{fmt.Sprintf("(cannot check: %v)", err)}
	}
	var out []string
	for _, c := range challs {
		for _, d := range deployments {
			if deploymentOf(d, c) {
				out = append(out, describeDeployment(d))
			}
		}
	}
	return out
}

// ── Operations ───────────────────────────────────────────────────────────────

func (s *Server) ListOperations(ctx echo.Context) error {
	if ok, err := requireAdmin(ctx); !ok {
		return err
	}
	out := []api.Operation{}
	for _, op := range s.operations.List() {
		snap := op.Snapshot(-1) // no lines
		out = append(out, toAPIOperation(snap))
	}
	return ctx.JSON(200, out)
}

func (s *Server) GetOperation(ctx echo.Context, id string, params api.GetOperationParams) error {
	if ok, err := requireAdmin(ctx); !ok {
		return err
	}
	op, found := s.operations.Get(id)
	if !found {
		return apiError(ctx, 404, "Operation not found")
	}
	offset := 0
	if params.Offset != nil {
		offset = *params.Offset
	}
	return ctx.JSON(200, toAPIOperation(op.Snapshot(offset)))
}

func (s *Server) CancelOperation(ctx echo.Context, id string) error {
	if ok, err := requireAdmin(ctx); !ok {
		return err
	}
	found, running := s.operations.Cancel(id)
	if !found {
		return apiError(ctx, 404, "Operation not found")
	}
	if !running {
		return apiError(ctx, 409, "Operation %s already finished", id)
	}
	return ctx.NoContent(202)
}
