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

### Optional: Monitoring (Prometheus + Grafana)

Append the monitoring overlay to bring up Prometheus and Grafana alongside the instancer:

```bash
docker compose -f docker-compose.yml -f docker-compose.monitoring.yml up -d --build
```

| Service    | URL                    | Credentials  |
|------------|------------------------|--------------|
| Grafana    | http://localhost:3000  | admin / admin |
| Prometheus | http://localhost:9090  | —            |

Grafana is pre-provisioned with the **Galvanize CTF Instancer** dashboard covering deployment counts, operation durations, extension stats, worker queue metrics, HTTP API latency, and more. The instancer exposes Prometheus metrics on port **5001** (`/metrics`).

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

Admin endpoints (role `admin`) also manage the challenges themselves, see [Challenge repositories](#challenge-repositories). Every token must carry an expiry (`exp`).

## Challenge repositories

Galvanize owns its challenge folder (`challenge_dir`). Each top-level folder of it is a repository: a git clone made through the API, or a folder copied there by hand. The admin API lets any client (Zync, a script, curl) manage them without access to the server.

These endpoints need an admin token whose `scope` claim is `manage`. The CTF platform's server puts it in the tokens it uses for its own calls; the admin tokens a dashboard hands to browsers must not have it, so that they can manage instances but not clone repositories, run builds on the hosts or read challenge files.

| Endpoint | What it does |
|---|---|
| `GET /admin/repos` | Repositories with their git state (remote, branch or tag, commit, local changes), and the allowed clone sources |
| `POST /admin/repos` | Clone `{url, name?, ref?, force?}`. The URL must be inside one of `repositories.sources` (cloning is disabled without any). `force` replaces an existing folder once the new clone succeeded |
| `POST /admin/repos/{name}/pull` | Fetch the branch or tag the repository follows and reset the folder to it (local changes are discarded). Images are not rebuilt: build them with `/admin/build` |
| `DELETE /admin/repos/{name}` | Delete the folder. Refused while instances of its challenges exist |
| `GET /admin/challenges[?repo=]` | Every challenge definition, whatever its type, with its full content, its repository's `commit`, its declared `files` and what building it means (`build`); definitions that could not be read are listed in `skipped` |
| `GET /admin/challenges/file?path=[&checksum=true]` | A file of a challenge folder (or its SHA-1 and size). Paths into `.git`, or leading outside the challenge folder, are refused |
| `POST /admin/build` | Build images `{repo?, challenges?, no_cache?}` on every Ansible host. The result lists the definition paths `built`, `failed` (refused builds included) and `skipped` (nothing to build) |
| `POST /admin/images` | `{action: status\|remove, repo?, challenges?}`: which images exist on each host, or remove the built ones (pulled images, images another challenge folder uses and platform images are never removed; refused while instances use them). The result maps each image to its state per host (`images`: `present`, `absent`, `removed`, `failed` when the removal failed, or `unreachable`/`error` on a host that did not answer or where the check failed) and each definition path to its images (`challenges`) |
| `GET /admin/operations[/{id}?offset=]` | Clones, pulls, removals, builds and image actions run in the background, one at a time: their status, result and log (from line `offset`) |
| `POST /admin/operations/{id}/cancel` | Cancel a running operation |

After a clone, pull or removal, the challenge index is reloaded.

Challenges are named by their definition file, relative to `challenge_dir` (`path` in the listing): `challenges` of a build or image request is a list of `{path}` (or `{category, name}`), and results use these paths.

**Declared files.** Each entry of a definition's `files` list is looked up in the challenge folder, then in its `files/` subfolder. The listing gives, for each one, `{declared, path, size}` (`path` to download with `/admin/challenges/file`), or an `error`: not found, absolute path, path leaving the challenge folder (symlinks included) or going through `.git`, or a Git LFS pointer (the repository was cloned without `git-lfs`). The listing does not hash files: ask `checksum=true` for the files whose SHA-1 you need.

**Commit.** `commit` is the repository's current commit: a client comparing it before and after a long task knows whether the repository changed in between (e.g. a pull).

**Sources.** A `url_prefix` is a URL ending with `/` (`https://github.com/your-org/`). A clone URL is allowed when it has the same scheme, host and port and its path continues the prefix's folders: `https://github.com/your-org-evil/x`, `https://github.com.evil.tld/your-org/x` and `https://github.com/your-org/../other/x` are refused, and so are URLs with credentials, a query, percent-encoding or `.`/`..` segments. Schemes: `https`, `http`, `ssh`, `git` and `file` (`git://` and `http://` are unauthenticated and unencrypted: keep them for a lab network).

**Credentials.** For a private repository, set `token` on its `https` source in `config.yaml`. Git receives it through its environment for each command, as a header scoped to the source's URL (an LFS server named by a repository's `.lfsconfig` does not get it, and redirects are not followed); it never appears in a URL, a command line, the API or the clone's `.git/config`. Clients cannot pass credentials: a URL holding some is refused. Git runs without hooks or fsmonitor.

**Builds** run where the instances run: Galvanize sends the challenge folder (without `.git`, and without symlinks leading outside it) to every Ansible host and runs Docker there, so builds use BuildKit and Compose and a deployment finds its images locally (the playbooks deploy with `pull: missing`):

- a challenge deployed from one image (`http`, `tcp`...) is built from the first `Dockerfile*` of its folder (`Dockerfile` preferred), tagged `deploy_parameters.image`. Without a Dockerfile, the image is taken to come from a registry (pulled at deployment) and there is nothing to build;
- a `custom_compose` challenge has its services with a `build` section built, from its Compose file when there is one (`deploy_parameters.compose_file`, else `compose.yaml`, `compose.yml`, `docker-compose.yaml` or `docker-compose.yml`; it must stay inside the challenge folder, symlinks included), even with an inline `compose_definition`, which usually references the images the file builds; otherwise from the inline definition. When both exist, a service must have the same image in both, or the build is refused. A built service without `image` is tagged `galvanize/<category>-<challenge>-<service>-<hash>:latest` (the hash of the exact names keeps two challenges apart even when their names give the same slug), and the index gives it the same name for deployments.

What a repository may build is limited, since anyone who can push to it decides what runs on the hosts:

- a `build` section may only use `context`, `dockerfile`, `dockerfile_inline`, `args`, `target`, `labels`, `shm_size`, `no_cache`, `pull` and `platforms`; contexts and Dockerfiles are relative paths inside the challenge folder (no host network, secrets, SSH agent, extra contexts, host caches or extra tags);
- image, service and Dockerfile names must be plain names;
- a challenge may not build an image another challenge folder deploys, builds or builds `FROM` (challenges of one folder share theirs), nor a platform image (`build.protected_images`; by default Galvanize's images, Traefik, Redis, MariaDB, MySQL and PostgreSQL: list your CTF platform's images there too, the list replaces the defaults). A local image of that name would replace the real one for everyone: deployments use `pull: missing`.

The listing shows refused builds with `build.refused` and the reason, and `build.buildable` tells whether building the challenge builds anything (it is deployable and its plan is `image` or `compose`). A build request may name challenges with nothing to build or refused: they come back as `skipped` or `failed`. Values sent to the build playbooks are marked unsafe for Ansible, so a name holding `{{ ... }}` is never rendered.

The build and image playbooks are part of the binary (they do not need to be in `ansible_dir`).

**Trust.** Cloning a repository means trusting everyone who can push to it: its challenges' Dockerfiles run on every Ansible host, and their deployment parameters reach the deployment playbooks. Allow only repositories whose writers you trust as much as the hosts' administrators, and keep Galvanize's API off the Internet when you can (only the CTF platform's server needs it for these endpoints).

## Challenge Definitions

Example challenge files (one per playbook type):

- `data/challenges/example/http/challenge.yml`
- `data/challenges/example/tcp/challenge.yml`
- `data/challenges/example/custom_compose/challenge.yml` (with a standalone `docker-compose.yml`)

Key fields:

- `name`, `author`, `category`
- `playbook_name`: `http`, `tcp`, or `custom_compose`
- `deploy_parameters`: image, ports, env, or Compose definition
- `flags`, `value`, `description`, `tags`

### Multi-service / Docker Compose challenges

For challenges that need more than one container (databases, sidecars, custom
networks/volumes), drop a standard Docker Compose file next to your
`challenge.yml`:

```
data/challenges/web/my-chall/
├── challenge.yml
└── docker-compose.yml
```

Galvanize auto-detects the first of `compose.yaml`, `compose.yml`,
`docker-compose.yaml`, or `docker-compose.yml` in the challenge directory,
validates it, and deploys it. When a compose file is present, `playbook_name`
defaults to `custom_compose`, so a minimal `challenge.yml` is enough:

```yaml
name: my-chall
author: YourName
category: web
type: zync

deploy_parameters:
  unique: false
```

This lets you develop and test the stack locally with a plain
`docker compose up`, then ship the file unchanged.

#### Exposing services (`expose`)

Compose challenges no longer need hand-written Traefik labels, manual network
wiring, or hand-picked host ports. Declare an `expose` block and Galvanize wires
the networking for you — the same automation the `http`/`tcp` playbooks provide
for single containers:

```yaml
deploy_parameters:
  unique: false
  expose:
    - service: web      # HTTP via Traefik → auto domain + SSL
      port: 80
      type: http
    - service: ssh      # raw TCP → published host port
      port: 22
      type: tcp
      scheme: ssh       # optional, only affects the rendered connection URL
```

For each entry Galvanize will, on deploy:

- `type: http` — attach the service to the external Traefik network, add the
  `traefik.enable` + router/service labels, and route
  `https://<project>.<instancer_host>/` to it (when more than one HTTP service
  is exposed, each gets a `<service>-<project>.<instancer_host>` subdomain).
  Requires `traefik_network` in `instancer.extra_deployment_parameters`.
- `type: tcp` — publish the container port. With
  `instancer.randomize_published_ports` enabled, the host port is randomized per
  team and persisted, so instances don't collide.

Connection info is returned automatically from the resulting Traefik route or
published port.

Notes:

- Use `deploy_parameters.compose_file: <name>` to point at a differently named
  file (relative to the challenge directory).
- An inline `deploy_parameters.compose_definition` string still works and takes
  precedence over any compose file, so existing challenges are unaffected.
- `expose` is optional: if you prefer, you can still hand-write Traefik labels
  and `ports:` in the compose file and omit `expose` entirely.
- The project name is generated automatically per team; do not set
  `container_name` in services or instances will collide.
- Image `build:` contexts and host bind mounts reference paths on the **target
  deploy host**, not the instancer — prefer pre-built images.

## Troubleshooting

- If deployments fail, verify SSH access to the target host and that Docker is installed.
- Ensure the mounted SSH key path in `docker-compose.yml` matches `instancer.ansible.private_key`.
- Check container logs with `docker logs galvanize-instancer`.
