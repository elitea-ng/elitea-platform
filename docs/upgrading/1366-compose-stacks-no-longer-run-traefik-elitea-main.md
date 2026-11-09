---
title: "Compose stacks no longer run Traefik; elitea-main is the browser edge"
issue: 1366
date: 2026-10-21
order: 14
---

**Affects:** local and CI compose stacks only: `deploy/docker-compose.yml`,
`deploy/docker-compose.standalone-full.yml` and
`deploy/docker-compose.e2e-standalone.yml` (#1359). Kubernetes installs are
unaffected: the Gateway API HTTPRoute (`deploy/gateway-api/httproute.yaml`)
still routes `/app` and `/docs` to elitea-web and terminates TLS, and the chart
never sets the new variable.

**What changed.**

* The `traefik` service and `deploy/traefik/` are gone. elitea-main publishes
  its own `:8080` listener on the port Traefik used, so browser origins stay
  the same:

  | Stack | Browser origin | Override |
  |---|---|---|
  | `docker-compose.yml` | `http://localhost:8080` | (fixed) |
  | `docker-compose.standalone-full.yml` | `http://localhost:8084` | `STANDALONE_PORT` |
  | `docker-compose.e2e-standalone.yml` | `http://localhost:8082` | `E2E_PORT` |

* `ELITEA_WEB_UPSTREAM` (e.g. `http://elitea-web:80`) turns on elitea-main's
  compose edge (`internal/api/webedge`). It relays `/app…` and `/docs…` to
  that origin, except the paths elitea-main serves under `/app`
  (`/app/<n>/mcp[/…]`, `/app/application_icon/`, `/app/application_tool_icon/`),
  redirects `/` to `/app/` with a 302 that keeps the query string, and answers
  404 for `/readyz` and `/startupz` to anything but a loopback caller. The
  client identity headers are stripped by elitea-main itself, as before.
  Unset, nothing changes: elitea-main serves its API and nothing else.
* The relay is plain HTTP. It carries browser traffic, not a service call.

**What to do.**

* Recreate the stack (`podman compose ... up -d --remove-orphans`, or
  `deploy/scripts/standalone-stack.sh up`) so the old `traefik` container is
  removed. Nothing else changes for a browser, Playwright or the OIDC
  redirect URIs, because the published ports are the same.
* A custom compose file that used `deploy/traefik/dynamic*.yml` must set
  `ELITEA_WEB_UPSTREAM` on elitea-main and publish its `:8080` instead.
* Do not set `ELITEA_WEB_UPSTREAM` in a cluster. (Superseded by "One edge
  route" above: the chart now sets it itself.)
