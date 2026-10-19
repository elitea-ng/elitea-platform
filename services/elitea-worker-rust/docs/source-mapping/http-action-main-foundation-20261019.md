# HTTP action Main foundation (T-5D step 5d-1)

| Item | Value |
| --- | --- |
| Date | 2026-10-19 |
| Branch | `feat/http-action-main-foundation` (from `origin/main` `ddc92a3cf`) |
| Brief | `handoffs/point5-wave1-20261008/T-5D-http-node.md` §4 "5d-1 Main foundation" (local handoff) |
| Design | `services/elitea-worker-rust/docs/http-action-node-design.md` (updated: "Implementation status (5d-1)") |
| Closes | FU-073 (transport port and egress guard); the Main half of FU-072. The `http` node is admitted in 5d-2. |
| Components | Go Main (`services/elitea-main`), Web (`apps/elitea-web`), contracts (`libs/jsonschema/runtime/v1`). No Rust change. |

## Business behaviour and what was not ported

There is no current-platform reference. Legacy EliteaUI has no HTTP node, and the legacy SDK reaches HTTP only through
the openapi/postman toolkits without an SSRF guard. Nothing of that path was ported. The only reuse is the existing
`openapi` configuration type as the credential source (API key Bearer/Basic/custom header, OAuth2 client
credentials), resolved by Main from the project vault. The admin page follows the existing Settings > Webhooks page
(same table, dialog, switch and toast components; no new visuals).

## Decisions (D1-D6, brief §8; accepted in PLAN.md §8.7, each kept in one place)

| # | Decision | Where it lives |
| --- | --- | --- |
| D1 | Revision 1 deleted (snapshot v2, invocation v3 only) | `internal/application/httpaction/frozen.go` (`SnapshotSchema`), `contract.go` (`Schema`) |
| D2 | Effect classes: GET/HEAD/OPTIONS I; other methods C unless the rule says `replay: idempotency_key` and the node binds a key (I) | `internal/application/httpaction/rule.go` `Rule.Retryable`, enforced again in `repos/http_action_effects.go` Begin |
| D3 | Auth in v1: none, Bearer, Basic, custom header, OAuth2 client credentials via `openapi` configurations; delegated OAuth and mTLS refused | `internal/infra/db/repos/http_action_credentials.go` `headers` |
| D4 | Permissions `models.http_action_rules.manage` (admin) / `.list` (admin, editor, viewer) | `internal/api/v2/httpactions` constants; grants in `migrations/shared/0170_*.sql`; Web `PERMISSIONS.httpActionRules` |
| D5 | Pool per Main replica, in process: 32 slots, 8 per execution | `internal/infra/storage/runtime_http_action.go` `MaxHTTPActionSlots`, `MaxHTTPActionsPerExecution` |
| D6 | Origins/paths only on the admin rules page; the editor endpoint returns names and enabled only | `GET /httpactions/prompt_lib/{projectID}/rule-names` |

## §8.1 (no flags)

There is no switch. `HTTP_ACTION_INTEGRATION_READY` does not exist and was not added. The
`runtime-context/http-actions` route is live (mTLS claim, full authority) wherever agent dispatch and the
configurations vault are composed (`internal/runtimecomposition/composition.go`, `http_actions.go`); it has no Worker
caller until 5d-2. That is a route, not a node family: `type: http` is still refused at save and start by
`pipelinelimits` until 5d-2 admits it in Worker, Main and Web together.

PLACEHOLDER-REST
