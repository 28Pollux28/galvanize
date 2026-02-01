# Galvanize Instancer

Galvanize Instancer is a lightweight service that deploys on-demand CTF challenge instances via Ansible and Docker. It is designed to work alongside the Zync CTFd plugin (https://github.com/28Pollux28/zync) and provides a simple HTTP API for deploy, status, extend, and terminate workflows.

## Highlights

- Per-team challenge instances with TTL and extension controls
- Ansible playbooks for HTTP, TCP, or custom Compose deployments
- Simple YAML challenge definitions matching CTFd challenge format
- JWT-protected API for easy integration with CTF platforms

## How It Works

- The Instancer service runs in a container and exposes an HTTP API.
- Challenges are defined in `data/challenges/*/challenge.yml`.
- Ansible playbooks in `data/playbooks/` handle deployments on your target hosts.
- Target hosts must be reachable over SSH and have Docker installed.
- HTTP challenges use Traefik to automatically set the domain name and SSL.

## Quick Start (Docker Compose)

1. Create your configuration file:

```bash
cp config.example.yaml config.yaml
```

2. Edit `config.yaml` to set:

- `auth.jwt_secret`
- `instancer.instancer_host`
- `instancer.ansible.inventory`
- `instancer.ansible.user`
- `instancer.ansible.private_key`

3. Update the SSH key mount in `docker-compose.yml`:

```yaml
    volumes:
      - /path/to/your/ssh/key:/home/galvanize/.ssh/ansible-ssh:Z,ro
```

4. Start the service:

```bash
docker compose up -d --build
```

5. Check health:

```bash
curl -f http://localhost:8080/health
```

## Zync CTFd Plugin Integration

Configure the Zync plugin to use your Instancer base URL and the same JWT secret you set in `config.yaml`.

- Base URL example: `http://your-instancer-host:8080`
- JWT secret: `auth.jwt_secret` in `config.yaml`

Refer to the Zync plugin documentation for the exact configuration fields.

## API Overview

The API is documented in `galvanize-instancer/api/openapi.yaml`. Main endpoints:

- `POST /deploy`
- `GET /status`
- `POST /extend`
- `POST /terminate`
- `GET /health`

All endpoints except `/health` require a JWT bearer token. Tokens are generated automatically by the Zync plugin.

## Challenge Definitions

Example challenge file:

- `data/challenges/example/challenge.yml`

Key fields:

- `name`, `author`, `category`
- `playbook_name`: `http`, `tcp`, or `custom_compose`
- `deploy_parameters`: image, ports, env, or Compose definition
- `flags`, `value`, `description`, `tags`

## Troubleshooting

- If deployments fail, verify SSH access to the target host and that Docker is installed.
- Ensure the mounted SSH key path in `docker-compose.yml` matches `instancer.ansible.private_key`.
- Check container logs with `docker logs galvanize-instancer`.
