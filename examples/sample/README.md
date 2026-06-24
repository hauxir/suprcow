# suprcow sample app

A minimal two-service app that demonstrates suprcow end to end:

- **web** — a [Vite](https://vitejs.dev) dev server (so you get HMR) that fetches
  a greeting from the API and renders it.
- **api** — a tiny zero-dependency Node HTTP server with `/api/hello` and `/health`.

It's wired **same-origin** (see [`preview.yml`](preview.yml)): the frontend is the
default host and `/api` + `/health` are routed to the api service on the *same*
host — no CORS, no second subdomain.

## What it shows off

- **Lazy spawn** — the stack boots on the first request to the PR's URL, then
  idles back to zero.
- **HMR auto-pull** — push a change to `web/src/app.ts` and the preview
  hot-reloads in place (no rebuild, no restart). Change `web/package.json` and
  suprcow does a full rebuild instead — that's the `rebuild_on` logic.
- **Same-origin routing** — `/api/hello` and `/health` reach the backend without
  CORS or a separate API host.

## Try it

1. Put this directory in a GitHub repo and point a suprcow daemon at it (see the
   top-level [README](../../README.md) and [`deploy/`](../../deploy)). For a quick
   public demo the config sets `auth: { disabled: true }`; remove that to require
   GitHub repo access.
2. Open a PR, visit `https://pr-<n>.<your-domain>/`, and watch it spin up.
3. Edit the `HEADLINE` constant in `web/src/app.ts`, push, and watch the open
   preview hot-reload within a second — no rebuild.

## Run it standalone (without suprcow)

```bash
docker compose up --build
# then browse the web container's Vite server; /api is proxied to the api service
```
