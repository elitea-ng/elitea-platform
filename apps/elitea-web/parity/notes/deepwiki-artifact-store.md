# Where DeepWiki's wiki content actually lives

**Phase P2.P0 of the DeepWiki UI port. Answered 2026-09-01.**

The plan named a kill-condition for this phase: *does the provider write wiki
artifacts into the same object store `/artifacts/objects/*` reads?* If yes, the
native UI appends to the existing modern artifact hooks; if no, it falls back to
hand-registered rows.

**The kill-condition fires, and harder than the plan anticipated.** Neither the
provider nor the vendored UI speaks to any route elitea-main serves.

## What the two sides speak

Both the provider's artifact client and the vendored UI use one path family:

```
POST   /api/v2/artifacts/artifacts/default/{projectId}/{bucket}
GET    /api/v2/artifacts/artifact/default/{projectId}/{bucket}/{name}
DELETE /api/v2/artifacts/artifact/default/{projectId}/{bucket}?filename=...
```

`apps/deepwiki-ui/src/DeepWikiApp.jsx` — the live root, imported by `main.jsx`
— contains **15** such call sites.

## What elitea-main serves

Three families, and none of them is that one:

```
/artifacts/buckets/{projectID}[/{bucket}]
/artifacts/objects/{projectID}/{bucket}[/{key}]      ← the modern surface
/artifacts/grants/{projectID}/...
/artifacts/s3/{bucket}[/*]                           ← SDK compatibility
```

Checked six ways, because a negative result is exactly the kind that gets
believed without checking the checker:

1. every `"/artifacts…"` string in the route registrations under
   `services/elitea-main/internal/api/` — no match;
2. every `^  /artifacts…:` path key in `api/openapi/v2.yaml` — no match;
3. a plain grep for `artifact/default` and `artifacts/default` across the Go
   service — no match;
4. `deploy/traefik/dynamic.yml`: on the standalone stack, `PathPrefix('/api/')`
   goes to elitea-main and nowhere else;
5. `deploy/docker-compose.yml` carries no pylon main runtime — only
   `pylon-indexer`, the index-plane shim;
6. the vendored UI's calls are same-origin by construction: `deepwikiui` injects
   `base_url: ""` and sets `connect-src 'self'`, so they reach elitea-main.

## Why it works today and where it does not

The retired `deploy/centry-hybrid/traefik/index-routes.yml` routed only
`^/api/v2/artifacts/buckets/[1-9][0-9]*$` and `^/artifacts/s3/…` to the Go
service. Everything else under `/api/v2/` falls through to pylon — which is
where this path family is implemented.

So:

| stack | vendored UI's artifact calls |
| --- | --- |
| hybrid (pylon present) | served by pylon |
| standalone / the Go target platform | **404** |

That is a shipped defect, not a finding about the port: the DeepWiki UI landed
in P4 reading routes the target platform does not serve. It is filed separately;
this note exists to record why the native port must not follow it.

## What this decides

**The native feature reads and writes through `/api/v2/artifacts/objects/…`.**

The plan already preferred this, for reasons that still hold — the endpoints
manifest carries the hooks, MSW fixtures exist, `:batchDelete` is better than a
per-file delete loop, and no v2.yaml change is needed. What the proof adds is
that it was never a preference between two working options. The legacy family is
not a fallback; it is a 404 on the platform this UI ships on.

The fallback the plan named — hand-registered rows for legacy paths — is
therefore **withdrawn**. There is nothing to register.

`DWIKI-004` records the legacy behaviour as waived, with this note as its
evidence, so the retirement is visible in the manifest rather than implied by
the absence of an item.

## What is still open

The provider writes wiki content through the same dead path family
(`artifacts_platform_client.py` in the Python engine, which the Rust
`services/elitea-deepwiki-engine` has since replaced; this note predates that).
Moving the UI to `/artifacts/objects/…` is necessary and not sufficient: the
provider has to write where the UI reads. That is provider work, outside this
track, and it blocks DWIKI-002, DWIKI-003, DWIKI-009 and DWIKI-011 from reaching
`verified` no matter how the UI is written.
