# Current `index_types` parity evidence

Status: source-complete, production-composed behind the explicit
`ELITEA_INDEX_TYPES_ENABLED` gate, integration-tested, and DEPLOYED since issue
#394 by `deploy/helm/elitea/values-standalone.yaml`,
`deploy/docker-compose.standalone-full.yml` and
`deploy/docker-compose.e2e-standalone.yml`.

This route is the ONLY handler for its path. #394 deleted the prototype
fallback from `internal/api/router.go` and the `Handler.IndexTypes` method from
`internal/api/v2/toolkits`.

`deploy/helm/elitea/values.yaml` ships the flag EMPTY, and the chart derives it
(`elitea-main.indexTypesEnabled`): ON where the install authenticates — a Form
authentication document or an OIDC issuer — and OFF where it does not. The
composition root asks `productionAuthenticationComposed` for the same reason,
so an OIDC-only install composes the route. An install with NO authentication
still refuses the capability, in the chart and in the binary.

## What #394 changed

The route answered the three Pylon maps only. The published contract for the
same path is `DocumentLoadersResponse` — `{items, total}` — so the generated
`apps/elitea-web` client read a body with no `items` key, and
`shared/api/unwrap.ts` reports that as an unrecognised shape. That drift is why
the flag stayed dark in every deployment.

The body now carries BOTH halves, the way #395 did it for the attached-skills
read: `items` and `total` beside `document_types`, `image_types` and
`code_types`. Each `items` entry names one category and lists that category's
extensions, sorted, so the two halves are the same snapshot read twice and
cannot disagree. Extra keys are contract-legal — `DocumentLoadersResponse` does
not close its object, so a client generated from the published spec ignores
what it did not ask for, and `api/openapi/v2.yaml` needs no edit.

## Observable contract

| Concern | Current application evidence | Go target | Result |
| --- | --- | --- | --- |
| HTTP route | `elitea_core/api/v2/index_types.py` registers `GET /api/v2/elitea_core/index_types/prompt_lib/{project_id}`. | `CurrentIndexTypesRoute` | Exact method, mode and path shape. |
| Permission | `index_types.py` requires `models.applications.index_types.details` for project admin, editor and viewer roles. | `CurrentIndexTypesPermission`; existing PostgreSQL RBAC resolver | Exact permission; project membership, suspension and central-role non-inheritance are tested against PostgreSQL. |
| Success body | The current handler returns `module.index_types` directly. | `CurrentIndexTypes` inside `currentIndexTypesResponse` | Exact top-level `document_types`, `image_types`, `code_types` maps, beside the published `items` and `total` keys (#394). The incompatible prototype `{index_types:[...]}` shape is rejected by contract tests. |
| Snapshot producer | `indexer_worker/methods/indexer_file_loaders.py:file_loaders_request` projects `document_loaders_map`, `image_loaders_map`, and `code_loaders_map`/`code_extensions`. | `sync_index_types_snapshot.py` and `CurrentIndexTypesSnapshot` | Generated from the worker-lock SDK revision; 18 document, 5 image, and 42 code entries. No partial Go list. |
| Image compatibility | The current producer reads `image_loaders_map`, not `image_loaders_map_converted`. | Generated snapshot excludes `.bmp` and `.svg`. | Exact current behavior. EliteaUI independently adds `.svg` in `fileTypes.js`; the API must not silently widen during this port. |
| UI consumer | `EliteaUI/src/api/applications.js:getDocumentLoaders`, `hooks/useFileTypes.js`, and `slices/fileTypes.js` consume the three maps directly. | `testdata/current_index_types_ui_response.json` | The three maps decode out of the response equal to the fixture. Byte-for-byte whole-body equality ended with #394, which added the published `items`/`total` keys the second shipped client needs. |
| Tenant boundary | The payload is process-global, but access is project-scoped by the current decorator. | Auth and PostgreSQL permission resolution execute before snapshot read. | Cross-project membership, suspended project/user, wrong permission, viewer/editor, and platform-admin cases are integration-tested. |
| Production ownership | The current Pylon handler remains the owner until an individual route is cut over. | `ELITEA_INDEX_TYPES_ENABLED` is strict and defaults to disabled; disabled composition registers no Go route, while enabled composition registers exactly the reviewed GET path. | Atomic ownership: the Go route either owns the exact path or falls through as absent. OFF is not free — `internal/api/router.go` then answers the path from the toolkits handler's static six-loader list, which no data backs. |

## Snapshot identity and bounds

- Worker lock SDK revision:
  `a78d3654f99d8ff89ca7233f20a66d676e564f79`.
- SDK constants source digest:
  `sha256:518049fb0ad8e8bf7030af2da64924a38684e2f9cbe037d35cec523d4b327bfd`.
- Canonical response digest:
  `sha256:f872b2f235c72836e85724cd0d49bcb030a567bf378ba6a0efe1e5768751e244`.
- Loader bounds: 64 KiB snapshot, 256 total category entries, 32-byte
  extensions, and 128-byte MIME values.
- Startup rejects partial, unknown-field, oversized, digest-mismatched, or
  malformed snapshots. Request handling copies the 65-entry immutable snapshot
  so callers cannot mutate concurrent responses; it starts no goroutines and
  owns no runtime cache or refresh loop.

The generator reads the exact Git object selected by
`services/elitea-worker-python/elitea-sdk.lock.json`; it does not require the
SDK worktree HEAD to equal that revision and does not import or execute SDK
modules. `--check` fails when either the embedded snapshot or the full UI
fixture is stale.

The audited current Centry `sdk_plugin` installation declares Elitea SDK
`0.8.30`; its installed `constants.py` has the same source digest above. The
new worker remains pinned to `0.8.26`, so dependency-version reconciliation is
a broader worker cutover gate, but it does not create `index_types` data drift
for this snapshot.

## Verification

- Generator parser tests cover full named-source extraction, converted-image
  exclusion, duplicate/partial definitions, and non-execution of SDK source.
- Go unit/contract tests cover the exact route/permission/body, authentication
  and authorization ordering, failure sanitization, nil/empty compatibility,
  snapshot validation, detached maps, and concurrent reads.
- `go test -race` passes for the route and snapshot packages.
- A disposable PostgreSQL HTTP integration matrix passes for project viewer,
  project editor, second-project viewer, cross-project denial, wrong
  permission, suspended principal, suspended project, and platform-admin
  non-inheritance through the production `NewRouter` composition. Tenant
  canaries are not returned.
- Gate tests reject ambiguous values and prove that unset, empty and `false`
  settings leave the route disabled. Production-router tests prove exact
  gate-on method/path ownership and gate-off absence.
- The production router's global OpenTelemetry middleware covers this route.
  This service has no route-local OpenAPI generation/registration pattern, so
  no separate OpenAPI artifact is introduced by this composition-only change.

## Deliberate safe differences and remaining cutover gates

The current Pylon module is briefly initialized with `{}` and can fall back to
three empty maps after an SDK import failure. When mounted, Go composition must
instead load the pinned complete snapshot and fail startup if that evidence is
invalid. This avoids serving a silently partial capability catalog. If an
already-composed reader fails during a request, the Go handler returns a
generic `500 {"error":"Failed to get index types"}` without dependency details;
the current in-memory handler has no equivalent runtime failure branch.

Production composition loads and validates the pinned snapshot before serving
when `ELITEA_INDEX_TYPES_ENABLED=true`. It also requires the complete
production authentication graph and constructs the PostgreSQL RBAC resolver;
an invalid gate, missing authentication composition, invalid route dependency,
or invalid snapshot fails startup instead of exposing a partial route.

Live routing through Traefik with the gate explicitly enabled and a real signed
browser session remains a deployment checkpoint, not claimed evidence. That
checkpoint must compare the current and Go response bytes for the same project
and user, then confirm an unauthorized project is denied. No EliteaUI change is
required.
