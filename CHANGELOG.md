# Changelog

## vX.X.X (YYYY-MM-DD)

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