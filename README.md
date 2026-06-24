# suprcow
<img width="195" height="150" alt="Image" src="https://github.com/user-attachments/assets/5638e558-b4b2-4a34-82fb-2aaf35255220" />

**super copy-on-write preview environments** — *Compose · On-demand · Workspaces*

Give every pull request its own isolated, fully-running copy of your app at its
own subdomain. Stacks spin up **lazily on the first request** to their URL and
idle back to zero when nobody's looking. One small daemon, one Docker host, your
existing `docker-compose.yml`. No Kubernetes.

> The name: `supr` spotlights **PR**; **COW** = **C**opy-**O**n-**W**rite — the
> Docker overlay mechanism that makes each PR's clone cheap — with a wink at
> `apt`'s "Super Cow Powers."

---

## Why

Per-PR preview environments exist (Uffizzi, Coolify, Render review apps), but
they're either Kubernetes-shaped, a whole PaaS, or a paid SaaS. **suprcow** fills
a narrower gap:

- **single box, compose-native** — point it at a repo with a `docker-compose.yml`
- **lazy / scale-to-zero** — a stack only runs while its URL is being used
- **webhook-driven lifecycle** — open a PR → it's reachable; push → it auto-pulls;
  close → it's torn down (containers + volumes)
- **bounded** — a hard `max_running` cap with LRU eviction keeps the host sane no
  matter how many PRs are open

It's roughly *"[Sablier](https://github.com/acouvreur/sablier)'s scale-to-zero +
PR lifecycle + bring-your-own-compose."*

## How it works

```
        *.preview.example.com  (wildcard DNS → your host)
                   │
        ┌──────────▼──────────┐   wildcard TLS (Caddy + ACME DNS-01)
        │        Caddy         │   reverse_proxy everything → suprcow
        └──────────┬──────────┘
        ┌──────────▼──────────┐   • GitHub webhook receiver
        │       suprcow        │   • Host → (PR, service) routing
        │   (one Go binary)    │   • lazy `compose up` + health-gate + waiting page
        │                      │   • auto-pull, LRU evict, idle reap, teardown
        └──────────┬──────────┘
                   │ shared docker network (services by stable alias)
     ┌─────────────┼──────────────┐
     │ pr-123: web, api, db        │  each = `docker compose -p <project>-pr-<n>`
     │ pr-456: web, api, db        │  with its own copy-on-write volumes
     └─────────────────────────────┘
```

1. A `pull_request` webhook tells suprcow a PR exists (it records it — no build).
2. The first request to `pr-123.preview.example.com` triggers a checkout of that
   PR's SHA, renders config, `docker compose -p myapp-pr-123 up -d`, and waits for
   your health gates — showing a branded waiting page meanwhile.
3. Once healthy, suprcow reverse-proxies (HTTP + WebSocket) to the container.
4. **Pushing to the PR hot-reloads in place** — suprcow updates the files in the
   running worktree and your dev server's watcher (Vite, etc.) picks them up over
   the same connection. It only rebuilds when a dependency manifest, Dockerfile,
   or the compose file changed. This is the fast loop: a push, then the change is
   live a second later — no rebuild, no waiting page.
5. No traffic for `idle_timeout` → the stack is `stop`ped (volumes kept, warm
   restart in seconds). Over `max_running` → the least-recently-used stack is
   evicted first.
6. Closing the PR tears everything down, including volumes.

## Quickstart

**1. Add `preview.yml` to your repo.** Simplest form — one service per subdomain,
with `inject` wiring the frontend to the API:

```yaml
repo: github.com/me/myapp
compose: docker-compose.yml

expose:
  - { service: web, subdomain: "pr-{n}",     port: 5173 }
  - { service: api, subdomain: "api-pr-{n}", port: 4000 }

inject:
  web:
    env:
      VITE_API_URL: "${PREVIEW_URL(api)}"   # https://api-pr-123.preview.example.com

health:
  api: { http: "/health", timeout: 180s }
  web: { tcp: 5173, timeout: 120s }

idle_timeout: 30m
max_running: 10
```

For a complete, runnable app wired **same-origin** (one host, `/api` folded onto
the frontend — no CORS), see [`examples/sample`](examples/sample).

**2. Validate it:**

```bash
suprcow validate preview.yml
```

**3. Run the daemon** (behind Caddy — see [`deploy/`](deploy/)):

```bash
docker network create suprcow
suprcow serve \
  --config preview.yml \
  --base-domain preview.example.com \
  --repo-url https://github.com/me/myapp.git \
  --data-dir /var/lib/suprcow
```

**4. Point a wildcard DNS record** `*.preview.example.com` at the host, and set
up the GitHub App (see [Access control](#access-control-on-by-default)) — it
delivers `pull_request` events and provides login + clone credentials.

Open a PR, visit its URL, watch it boot.

## `preview.yml` reference

| Field | Meaning |
|-------|---------|
| `repo` | git remote (used to clone/fetch PR branches) |
| `compose` | path to your compose file (default `docker-compose.yml`) — used as-is |
| `env_file` | optional dotenv injected into every stack |
| `expose[]` | services reachable externally: `service`, `subdomain` (must contain `{n}`), `port`, optional `routes` |
| `expose[].routes[]` | fold other services onto the same host by `method`/`path`/`path_prefix` → `service` + `port` (same-origin mode) |
| `inject{}` | per-service `env` and rendered `files` for cross-service wiring |
| `health{}` | readiness gate per service: `http` path or `tcp` port, plus `timeout` |
| `rebuild_on` | changed paths that force a rebuild instead of a hot reload (defaults to common lockfiles/Dockerfiles/the compose file) |
| `on_update` | commands run in a service after a push (e.g. `{ service: api, run: "npm run migrate" }`) |
| `reload_trigger` | HTTP endpoints suprcow GETs after a hot-reload to nudge a request-driven reloader (e.g. Phoenix `code_reloader`); `{ service, port, path }`. Needed when a backend only recompiles on request but is reached only over WebSocket |
| `idle_timeout` | inactivity before a stack is stopped (volumes kept) |
| `max_running` | hard cap on running stacks; LRU-evict beyond it |
| `auth` | access gate (**on by default**): `disabled`, `provider`, `repo`, `allow` (`collaborators`/`org-members`), `org`, `cookie_domain` |

### Template variables (in `inject` values/files)

| Variable | Resolves to |
|----------|-------------|
| `${PR_NUMBER}` | the PR number |
| `${BRANCH}` | the PR head branch |
| `${SHA}` | the commit being run |
| `${PREVIEW_HOST(svc)}` | bare external host of an exposed service |
| `${PREVIEW_URL(svc)}` | `https://` URL of an exposed service |

`inject` is the bit that makes "just hook your services in" work: it's how a
frontend learns its backend's *external* preview URL without hardcoding anything.

### Same-origin mode (`routes`)

Putting the frontend and its API on *separate* subdomains is simplest, but it
makes API calls cross-origin — which becomes painful once you front the previews
with SSO (the perimeter cookie has to be shared across subdomains, CORS must
allow credentials, and an expired session 302-redirects XHRs into failure).

To avoid all that, fold the API onto the **same host** as the frontend with
`routes`. The default `service` handles the host; `routes` divert specific
requests to other services:

```yaml
expose:
  - service: web
    subdomain: "pr-{n}"
    port: 5173                    # default: the frontend
    routes:
      - { path: "/gql-ws", service: api, port: 4000 }        # websocket
      - { method: POST, path: "/", service: api, port: 4000 } # GraphQL HTTP
```

Now everything is one origin: no CORS, no cross-subdomain cookies, and the
session cookie is sent on every request automatically. Point the frontend's API
config at its own host (`${PREVIEW_HOST(web)}`) and you're done — see
[`examples/sample`](examples/sample) for a real two-service app wired this way with
zero changes to the app's code.

## Access control (on by default)

suprcow is GitHub-native, so it uses GitHub as the source of truth for who may
see a preview: **by default a user can open a PR's environment only if they can
access the repo on GitHub.** The gate runs in front of the proxy, so
unauthenticated requests are rejected *before* they can trigger a spawn. It's
**secure by default** — the daemon refuses to start serving unless you either
provide GitHub OAuth credentials or explicitly opt out.

suprcow is a single **GitHub App** — one least-privilege identity that handles
webhooks, user login, *and* private-repo cloning (via short-lived installation
tokens, so no deploy key or PAT).

### Setup
1. Create a GitHub App (Settings → Developer settings → GitHub Apps):
   - Callback URL: `https://suprcow.preview.example.com/_suprcow/auth/callback`
   - Webhook URL: `https://suprcow.preview.example.com/_suprcow/hooks/github` + a webhook secret
   - Permissions: **Contents: Read** (clone), **Metadata: Read**, **Pull requests: Read**
   - Subscribe to **Pull request** events
   - Generate a **client secret** and a **private key**
2. **Install** the App on the repo (or org) you want previews for.
3. Give the daemon its credentials via env:
   ```
   SUPRCOW_GITHUB_CLIENT_ID=...                          # user login
   SUPRCOW_GITHUB_CLIENT_SECRET=...
   SUPRCOW_GITHUB_APP_ID=...                             # installation tokens → cloning
   SUPRCOW_GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/suprcow-app.pem
   SUPRCOW_WEBHOOK_SECRET=...                            # App webhook secret
   SUPRCOW_SESSION_KEY=$(openssl rand -hex 32)           # signs session cookies
   ```
4. Done — visiting any preview redirects through GitHub login; the session cookie
   is scoped to the parent domain, so one login covers every PR subdomain (and,
   in same-origin mode, every API/WS call). Private repos clone with a
   short-lived installation token.

The OAuth flow runs on a fixed control host (`suprcow.<base-domain>` by default,
`--auth-host` to override) because GitHub OAuth Apps allow a single callback URL.

### Authorization rule
```yaml
auth:
  allow: collaborators   # default — anyone who can access the repo
  # allow: org-members   # alternatively, anyone in the org
```

### Opting out
```yaml
auth:
  disabled: true         # previews open to anyone — only for fully public demos
```

The webhook endpoint stays ungated (it's HMAC-verified). Non-browser API clients
(CI smoke tests) can't do interactive SSO — point them at an opted-out instance
or add a token check.

## Status

Early but functional. Implemented: config + validation, host routing, lazy
spawn, same-origin routing, health gates, **HMR-first auto-pull** (hot-reload in
place; rebuild only on dependency/compose changes) with post-update hooks, LRU
eviction, idle reaping, teardown, GitHub App auth (repo-access gate, on by
default) with installation-token cloning of private repos, preview-compose
sanitization (rejects host-escape config — privileged, host bind-mounts, etc. —
since the daemon holds the Docker socket), webhooks, waiting page, Bolt-backed
state. On the roadmap: GitLab/Gitea support, build/dep cache
warming, a status dashboard, and a pluggable non-compose backend.

## License

MIT — see [LICENSE](LICENSE).
