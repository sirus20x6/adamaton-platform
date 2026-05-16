# deploy-agent

Push-deploy receiver that runs on every Adamaton host. The workstation's `bin/adam ship` calls it after pushing a freshly-built image to the workstation registry.

## Endpoints

| Method | Path | Auth | Behavior |
|---|---|---|---|
| GET | `/health` | none | Liveness probe. |
| GET | `/services` | bearer | MANIFEST allow-list + host name. |
| GET | `/status?service=X` | bearer | `docker compose ps X --format json`. |
| POST | `/restart?service=X&tag=Y` | bearer | Bump `ADAMATON_<X>_TAG` in `image-tags.env`, run `docker compose pull X && up -d X`. |
| POST | `/restart-all?tag=Y` | bearer | Same for every MANIFEST service. Slow path. |

## Config

| Env var | Default | Notes |
|---|---|---|
| `DEPLOY_AGENT_TOKEN` | (required) | Bearer for every authenticated endpoint. Constant-time compared. |
| `DEPLOY_AGENT_HOST` | (required) | Must equal `MANIFEST.yaml`'s `host`. Refuses to boot on mismatch. |
| `DEPLOY_AGENT_COMPOSE_DIR` | `/workdir` | Bind-mount of the host's `~/Adamaton-deploy/`. Holds `docker-compose.yml` + `image-tags.env` + `MANIFEST.yaml`. |
| `DEPLOY_AGENT_BIND` | `:9128` | Caddy fronts this. |

## Safety

- Bearer-required on every endpoint except `/health`.
- Service name + tag both regex-validated before reaching `docker compose`.
- One deploy at a time (mutex around every compose op).
- 5-minute timeout per subprocess.
- Refuses to redeploy itself — use `bin/adam ship-self <host>` for agent upgrades.

## Bootstrap

See `Adamaton/docs/PUSH_DEPLOY.md` for the per-host one-time setup (registry, daemon.json, initial agent bootstrap).
