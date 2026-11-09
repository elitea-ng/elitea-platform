---
title: "The X-Auth-* edge identity plane is removed; Form sessions resolve in elitea-main — BREAKING for external auth edges"
issue: 1366
date: 2026-10-21
order: 9
---

**Affects:** a deployment whose own edge (outside this repository) calls
`elitea-main`'s `/internal/auth/main` or `GET /auth` as a forward-auth target
(Traefik `forwardAuth`, a Gateway API `ExtensionRef`, NGINX `auth_request`),
or that sends `X-Auth-*` identity headers to `elitea-main` in any other way.
A deployment that uses only this repository's chart or compose files has
nothing to do.

**What changed.** (#1359)

- `GET /internal/auth/main`, `GET /auth` (the edge-auth token check) and the
  Form plane's `/auth/check` are gone and answer 404. No `elitea-main` route
  emits an `X-Auth-*` header any more, and none reads one.
- **Form sign-in sessions now resolve in `elitea-main` itself.** The
  `elitea_browser_auth` cookie that `/auth/form/authorize` sets is read
  in-process by every API route and the admin console, exactly like the single
  sign-on `elitea_session` cookie (including the cross-site write check). No
  edge ForwardAuth is needed for `ELITEA_FORM_LOGIN_ENABLED=true` — before
  this change, a Form sign-in authenticated nothing behind an edge that did not
  run one, which was every edge this repository ships.
- `elitea-main` deletes the inbound identity headers (`X-Auth-Type`,
  `X-Auth-ID`, `X-Auth-User-ID`, `X-Auth-Reference`, `X-Auth-Signature`,
  `X-Auth-Avatar`, `X-Auth-Avatar-State`, `X-Auth-Session-Id`,
  `X-Auth-Session-Name`, `X-Auth-Session-Endpoint`,
  `X-Elitea-Identity-Signature`, `X-Elitea-Project-Id`, `X-Elitea-User-Id`,
  `X-Elitea-Tenant-Id`) on every request before any route runs, so it no
  longer depends on an edge to strip them. `X-Elitea-Execution-Id` is kept: the
  desktop and the runtime worker send it on `/llm`, and `elitea-main` verifies
  it against a live execution of the caller before using it.
- The worker's private edge (`platform-edge`) no longer authenticates the
  execution-events stream: the worker's bearer token is read by
  `elitea-main` directly, as on every other route.

**What to do.** Remove any forward-auth middleware that points at
`/internal/auth/main` or `/auth`; with it in place every request is refused,
because the target answers 404. Route browser traffic straight to
`elitea-main` — it authenticates the Form cookie, the single sign-on cookie and
bearer tokens itself. Keep stripping the identity headers at the edge if you
like; it is now defence in depth rather than the only control. The auth
document is unchanged; its `mappers`, `authorization` and `credential_headers`
sections are still validated but no longer used.
