# contract/

The server API contract this app is built against. It is the **only** coupling
between Agent Zefir and the Elitea project: no Elitea source code is copied into
this repository.

| File | What it is |
|------|------------|
| `elitea-v2.yaml` | Verbatim copy of the server's published OpenAPI document. **Never edit it.** |
| `elitea-v2.yaml.sha256` | `shasum -a 256` of the copy; `npm run contract:verify` fails on any change. |
| `LICENSE.elitea` | The upstream Apache-2.0 license text and NOTICE, required to redistribute the copy. |
| `client-operations.json` | The app's declared API surface: the operationIds kept in the generated client. |
| `overlay/native-gap.yaml` | **Our own** OpenAPI fragment for REST operations the pinned contract does not define yet (conversation get/create/delete, transcript list, chat send, notifications list/update, and the ADR-0025 native-client operations listed below), plus `x-zefir-extend` additions to upstream operations and schemas (incremental sync). Each operation and addition records where its shape was observed (`x-zefir-source`) and what will replace it (`x-zefir-until: ADR-0025 WP5`). |

## Generated client

`scripts/contract.mjs` turns these inputs into `src/api/generated/elitea.gen.ts`
(committed, types only, consumed by `openapi-fetch`):

| Command | What it does |
|---------|--------------|
| `npm run contract:verify` | sha256 check of the upstream copy |
| `npm run contract:build` | verify, merge the overlay, prune to `client-operations.json` plus the `$ref` closure, write `contract/.build/merged.yaml` (git-ignored) |
| `npm run contract:gen` | build, then run `openapi-typescript` (pinned `7.13.0`) and write the generated file |
| `npm run contract:check` | regenerate in memory and fail if the committed file differs (CI) |

The build **fails** when the overlay defines an operation or component that
upstream already defines. When the server publishes the missing operations
(ADR-0025 WP5), bump the pin, delete the overlay entries the build names, and
regenerate. Server-sent-event endpoints are typed by hand in `src/api/sse/`.

## Provenance

| | |
|---|---|
| Repository | `elitea-ng/elitea-platform` |
| Commit | `e7585fee2578779b7e293be196182f8db42263a1` (`origin/main`, 2026-10-04) |
| Path | `services/elitea-main/api/openapi/v2.yaml` |
| SHA-256 | `56d6638fdcad882675464d031c2d29e1c19f0fe69e70dbf75f3cebce3658b551` |
| Size | 21,456 lines |
| License | Apache-2.0 (see `LICENSE.elitea`) |
| Copied | 2026-10-04 |

## Bumping the pinned contract

1. Copy the new file verbatim:
   `git -C <elitea-platform> show <commit>:services/elitea-main/api/openapi/v2.yaml > contract/elitea-v2.yaml`
2. Update the hash: `(cd contract && shasum -a 256 elitea-v2.yaml > elitea-v2.yaml.sha256)`.
3. Update the provenance table above (commit, date, hash, line count), and
   `LICENSE.elitea` if the upstream LICENSE or NOTICE changed.
4. Run `npm run contract:verify`, then regenerate and review the diff of the
   generated client.

## Native-client operations (ADR-0025, server branch)

The ADR-0025 operations are implemented on the server branch
`feat/adr-0025-native-client-support`, which is not on `main` yet. The pin
above is **not** moved to that branch. Instead the overlay describes the
operations the app uses, written against that branch at commit
`398985cdc882c4d35c0f13ff607f1212b3b1c2ab` (WP1 discovery and brand pack
JSON, WP2 native token and revoke, WP3 `device_revoked`, WP4 client policy and
`426`):

| operationId | Route |
|---|---|
| `getClientDiscovery` | `GET /.well-known/elitea-client` (origin root) |
| `getBrandingPackJSON` | `GET /api/v2/branding/pack.json` (references the upstream `BrandPack` schema) |
| `exchangeNativeToken` | `POST /api/v2/auth/native/token` (form body; code and refresh grants) |
| `revokeNativeToken` | `POST /api/v2/auth/native/revoke` (form body) |

Incremental sync (ADR-0025 WP6) was added to the overlay against the same
branch at commit `7665eb30f1d4b88eaac2eccb71af5fb6b72c53ae` (WP6: `changes_since`
cursors and tombstones), with the shapes as documented by the branch's
`v2.yaml` and `API_CONTRACT.md` at `04cab926be210784d9147f3753eb5ffe43241795`
(WP5; that `v2.yaml` has sha256
`5ad3c4ea805377bfa651e4ceae01ebaa6157fb33fdb50d8d06b34cd7236f4e9a`):

| Where | What |
|---|---|
| `listConversations` (upstream) | `changes_since` parameter, `400`/`410`, via `x-zefir-extend` |
| `ConversationListing` (upstream) | `tombstones`, `next_cursor`, `has_more`, via `x-zefir-extend` |
| `listConversationMessages` (overlay) | `GET /api/v2/elitea_core/messages/prompt_lib/{project_id}/{conversation}`, legacy page or delta |
| `listNotifications` (overlay) | `changes_since`, `410`, delta fields on `NotificationPage` |
| `SyncTombstone`, `SyncCursorError`, `SyncCursorRefused`, `SyncCursorExpired` | the shared delta shapes |

`x-zefir-extend` adds a parameter, a status or a property to an operation or
schema that the pinned upstream document already defines. Like an overlay
operation, each addition fails the build once upstream defines the same one.

`GET /api/v2/auth/native/authorize` is a browser navigation, not an API call,
so it is not typed; the app builds that URL itself (`src/auth/nativeAuth.ts`).

When a pinned release defines them, `contract:build` fails on each overlay
copy; delete those and regenerate.

## Non-affiliation

Elitea is a trademark of its respective owner. Agent Zefir is an independent
client and is not affiliated with or endorsed by it.
