# Changelog

## vX.X.X (YYYY-MM-DD)

### Changed
- Docker Compose project names, which also name the subdomains of instances, now include the category and a hash of the instance's identity (e.g. `polypwn-web-login-team1-<hash>`). Instances already running when upgrading keep their previous name: terminating them also removes the project under that name, so no containers are left behind
- `/admin/reload-challs` returns `{"indexed": <count>, "skipped": [{"path", "reason"}]}` instead of an empty body

### Fixed
- Deployments failing on Docker Compose 2.38+ with `can't set distinct values on 'pids_limit' and 'deploy.resources.limits.pids'`: the PID limit is now set as `deploy.resources.limits.pids`, next to the CPU and memory limits, instead of the service-level `pids_limit`. The `custom_compose` playbook drops a `pids_limit` set in the challenge's own compose file, so Galvanize's limit applies as before
- Same-named challenges in different categories no longer collide: the category is now part of the deploy and terminate check against the JWT (a token for `web/login` cannot deploy or terminate `pwn/login`), of the per-instance lock, and of the Docker Compose project name
- Compose project names end with a real hash of the instance's identity: the previous suffix was always `706f6c`, since `sha1.New().Sum(name)` appended the hash of nothing to the name instead of hashing it. Names are also cut to fit a 63-character DNS label
- One invalid `challenge.yml` no longer takes every challenge down: it stopped Galvanize from starting, or made `/admin/reload-challs` fail. Files that cannot be parsed (including a bad compose file), unreadable directories and challenges declaring an already indexed `category/name` are now skipped and logged, and the other challenges are indexed
- A missing or unreadable challenge directory makes `BuildIndex` return an error instead of panicking; a failed reload keeps the current index

## v0.7.1 (2026-06-02)

### Added
- Auto-detect a standalone Docker Compose file (`compose.yaml`, `compose.yml`, `docker-compose.yaml`, or `docker-compose.yml`) next to `challenge.yml` for multi-service challenges, so authors no longer have to embed the whole document as an inline `compose_definition` string
- Optional `deploy_parameters.compose_file` to point at a differently named compose file (relative to the challenge directory)
- Default `playbook_name` to `custom_compose` when a compose file is detected and no playbook is set explicitly
- `deploy_parameters.expose` block for compose challenges: automatically wire Traefik (http: external network + router/service labels + domain) and published ports (tcp: reuses per-team host-port randomization/persistence), removing the need to hand-write Traefik labels, attach networks, or pick host ports. Multiple HTTP exposures get per-service subdomains, and connection info is derived automatically
- Validate `expose` entries at challenge index time (structure + that referenced services exist in the compose definition)
- Document `traefik_network` in `config.example.yaml` (required for http challenges and http exposures)

### Changed
- `example/custom_compose` now demonstrates the standalone `docker-compose.yml` workflow with an `expose` block; inline `compose_definition` still works and takes precedence over a sibling file

## v0.7.0 (2026-03-18)

### Added
- Configurable container resource limits (CPU, memory, PID) with global defaults in `config.yaml` and per-challenge overrides in `deploy_parameters.resource_limits`
- Optional `http_port` in `deploy_parameters` to configure which container port Traefik routes HTTP traffic to (defaults to 80)
- Per-playbook example `challenge.yml` files (`example/http`, `example/tcp`, `example/custom_compose`)

## v0.6.0 (2026-03-14)
### Added
- Bake `data/playbooks` into the `galvanize-instancer` runtime image so default playbooks are available even without a host bind mount
- Support optional protocol hints for TCP `published_ports` (for example `22/ssh` or `8080:80/http`) and use the hint in generated connection info URLs
- Add optional global `instancer.randomize_published_ports` config flag to randomize host port bindings for non-fixed TCP `published_ports` entries

### Changed
- Normalize hinted `published_ports` before running Ansible so Docker Compose still receives valid port syntax while preserving protocol hints for connection string rendering
- Persist randomized host-port bindings in SQLite so deploy retries and instancer restarts reuse the same published ports until terminate
- Prune stale `published_port_bindings` rows on startup when no matching non-deleted deployment exists

## v0.5.6 (2026-03-02)
### Fixed
- Database initialization failure: automatically create parent directory for SQLite database file to prevent "out of memory (14)" error when `db_path` directory doesn't exist

## v0.5.5 (2026-02-20)
### Changed
- Optimize Docker builds with layer caching for Go dependencies
- Add GitHub Actions cache for Docker layers to speed up CI/CD builds

## v0.5.4 (2026-02-20)
### Added
- Modify bump version script to automatically tag and commit to master

## v0.5.3 (2026-02-19)
### Added
- Add `--version` / `-v` flag to display the current version

## v0.5.2 (2026-02-19)
### Fixed
- Pull policy prevented using local images

## v0.5.1 (2026-02-19)

### Fixed
- Variable redeclaration made sanitization of compose project name not work for non unique challenge names
- Small tweaks to grafana dashboard

## v0.5.0 (2026-02-18)

### Added
- Prometheus metrics endpoint on port **5001** (`/metrics`) with optional HTTP Basic Auth (`instancer.metrics.username` / `instancer.metrics.password` in config)
- Deployment count gauge (`instancer_deployments`) backed by the database, grouped by status / category / challenge / team
- Deploy and terminate operation counters (`instancer_deploy_ops_total`, `instancer_terminate_ops_total`) with `result` label
- Deploy and terminate duration histograms (`instancer_deploy_duration_seconds`, `instancer_terminate_duration_seconds`) with category / challenge / team labels
- Deploy conflict counter (`instancer_deploy_conflict_total`) for 409 responses
- Unauthorized deploy request counter (`instancer_unauthorized_deploy_requests_total`) per team
- Extension operation counter (`instancer_extend_ops_total`) and rejection counter (`instancer_extend_rejected_total`) with `reason` label (`window_not_reached`, `no_extensions_left`, `already_expired`)
- Deployment lifetime histogram (`instancer_deployment_lifetime_seconds`) recorded at termination time
- Worker job retry counter (`instancer_job_retries_total`) and permanent failure counter (`instancer_job_permanent_failures_total`) with `job_type` label
- Redis queue depth gauge (`instancer_queue_depth`) and queue wait time histogram (`instancer_job_queue_wait_seconds`) — only registered when Redis is configured
- Challenge index size gauge (`instancer_challenges_indexed`) per category, updated on startup and on challenge reload
- Grafana monitoring stack (`docker-compose.monitoring.yml`) with Prometheus + Grafana, pre-provisioned with a full dashboard covering all metrics
- `make monitoring-up` / `make monitoring-down` targets

### Fixed
- TCP playbook (and any playbook using `{{ env | default({}) }}`): challenges without an `env` key in `deploy_parameters` no longer fail with *"environment must be a mapping"* — `env` is now normalised to an empty map before being passed to Ansible

## v0.4.0 (2026-02-18)
- Add `/admin/team-deployments` endpoint to list all deployments grouped by team with deployment duration
- Add `/admin/error-deployments` endpoint to list all deployments in error status
- Add `/admin/retry-deployment` endpoint to retry failed deployments (deploy, terminate, or delete)
- Add `previous_status` field to track deployment status before transitioning to error

## v0.3.0 (2026-02-17)
- Add ansible worker and redis queue
- Add loadtest package to stress test restserver

## v0.2.2 (2026-02-10)
- fix action capital letter in repo name

## v0.2.1 (2026-02-10)
- fix go.mod go version
- fix logging issue on terminate challenge
- change docker-compose project name to `galvanize`

## v0.2.0 (2026-02-09)
- Rework project to use Dependency Injection
- Add tests for restserver, challenge packages
- Add `/admin/reload-challs` endpoint to reload challenge index
- Change port in serve command to be a flag `--port, -p` and use 8080 by default
- Edit endpoints to use hyphens instead of underscore
- Add Makefile

## v0.1.1 (2026-02-07)
- Add json error response to status 404 for unique challenges

## v0.1.0 (2026-02-04)
- Initial release
- Add `/admin/config_check` endpoint to check link with zync plugin
- Add `/admin/deploy` endpoint to deploy unique challenge
- Add `/admin/deploy_all` endpoint to deploy all unique challenges
- Add `/admin/terminate` endpoint to terminate unique challenge
- Add `/admin/terminate_all` endpoint to terminate all unique challenges
- Add `/admin/list_unique_challs` endpoint to list unique challenges
