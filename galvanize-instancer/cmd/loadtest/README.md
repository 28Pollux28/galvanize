# Instancer Load Test

Minimal CLI to stress test the instancer endpoints with concurrent deploy, status, and terminate requests.

## Usage

```bash
go run ./cmd/loadtest \
  --base-url http://localhost:8080 \
  --jwt-secret "your_jwt_secret" \
  --category web \
  --challenge http \
  --teams 500 \
  --concurrency 500
```

## Notes

- The tool signs a per-team JWT using the provided secret and the configured challenge/category.
- Default phases are `deploy,status,terminate`. Override with `--phases` if needed.
- The `status` endpoint uses the JWT claims to select the deployment, so the token must match the challenge and team.
