# Changelog

## vX.X.X (YYYY-MM-DD)

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