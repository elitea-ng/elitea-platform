# Current prompt-context read parity

This slice owns two current-baseline, read-only HTTP contracts. The
project-context `PUT` and `DELETE` contracts remain with the current
application and are not mounted here.

**Gating (updated by #194, flag removed by #367).** `main.go` composes these
routes whenever a credential exists (FormGraph or an OIDC session), reusing the
Configurations runtime's vault loader when `ELITEA_CONFIGURATIONS_ENABLED` is on
so a `ELITEA_VAULT_MASTER_KEY_FILE` deployment keeps unwrapping project keys
correctly.

`ELITEA_PROMPT_CONTEXT_READS_ENABLED` used to compose the same pair a second
time, inside the Configurations chain. #367 deleted it. No deployment set it,
and the ungated composition above builds the identical pair in every case the
flag could have applied to, so the flag could only pre-empt a block that was
going to run anyway.

Two claims this section used to make were wrong and are recorded here because
each was load-bearing for a decision:

- "that chain is set in no deployment" — stale.
  `deploy/docker-compose.standalone-full.yml` sets
  `ELITEA_CONFIGURATIONS_ENABLED: "true"`.
- "the production router mounts both" — false in code.
  `mountReviewedProductionRoutes` registers the chat-config path only, and its
  doc comment gives the reason: the project-context path is already served by
  the prototype eliteacore handler, whose default for `enabled` differs.

Which paths this makes reachable is therefore decided at the router, and only
the chat-config path is registered from this slice.

Authentication was relaxed to match `notificationsapi.
NewCurrentNotificationEventsRoute` (#152): `PrincipalValidator` and
`ForwardedIdentityVerifier` (since removed with the whole X-Auth-* plane, #1359)
are optional, because an OIDC-only deployment has no FormGraph and only a
session cookie. Authorization is unchanged — an unauthenticated request is
still rejected,
and the per-project permission is still resolved before either handler runs.

## Pinned source evidence

| Current source | SHA-256 | Behavior carried forward |
| --- | --- | --- |
| `elitea_core/api/v2/chat_config.py` | `2bf374344b90336b3251b5bdcbf85882f05fcbc4d482a7a54408bf3096cca68d` | GET path, permission, five keys, defaults, Python `int(value)` |
| `elitea_core/api/v2/project_context.py` | `e983d63c8bf3ea860f4e595f0edfa4f67bfd5b3ebf94630781d9b2814ec6f61e` | GET path, permission, five-second configuration call, project-only lookup |
| `elitea_core/models/pd/project_context.py` | `65b422f833974bd682917910cda921c33baa6c1dd5fa3a570deed0bd88ac8081` | exact `id`, `content`, `enabled`, `updated_at` projection and defaults |
| `shared/tools/vault_tools.py` | `193b16914e7c37038b335528b3c70fb25dd1dc956c5c4a75440a349bc9b1b3dd` | shared admin regular, project hidden, project regular merge order |
| `configurations/models/pd/configuration.py` | `58b21c5381d7deb304a5593d7528f89f2db6e7a5d15641bdbb73c188deb1110f` | upstream `ConfigurationDetails.data` requires an object before project-context projection |
| `configurations/rpc/getters.py` | `a93047fada262940bed6d0cb82fb9dada217bd44061c6b7ca8708d0ee2689e7e` | project-context RPC has no public/shared fallback |
| `configurations/utils_getters.py` | `42904ca561b7decec3479bc749be001fe5a4375d89899ed32423a1064c0f0bd6` | tenant query filters `project_id` and `type`, uses `LIMIT 1` semantics without ordering |

The checked-in static RBAC catalog still records project-context source hash
`c74644199a8ee0623083fe88990cc2f5aa1b867c4e103a4d47bb0bc0097e8229`
and omits the subsequently added `DELETE`. It must be regenerated in its
inventory-owner workflow. This read-only port follows the current source above
and deliberately does not expand scope to either mutation.

## Functional matrix

| Contract | Current behavior | Go owner and evidence |
| --- | --- | --- |
| `GET /api/v2/elitea_core/chat_config/prompt_lib/{projectID}` | requires `models.chat.conversation.details` in default/prompt-lib mode | `CurrentRoutes`; unit and real PostgreSQL HTTP tests deny missing membership, wrong permission, suspended user/project and cross-project access before vault reads |
| Secret precedence | project regular > project hidden > admin regular > default; admin hidden is not shared | `CurrentChatConfigVaultReader`; one admin and one project snapshot per request; precedence and encrypted PostgreSQL fixtures |
| Integer conversion | Python 3.12 `int`: booleans, binary64 JSON-float truncation, and decimal strings including signs, underscores and Unicode decimal digits; quoted float text and null fail; decimal conversion accepts 4300 digits and rejects 4301 | `centrysecrets.LookupPythonInteger`; Python-derived golden vectors include `1e100`, underflow, fractional signs, booleans, 4300/4301 ASCII and Unicode boundaries and malformed values. Signs and separators do not count toward the limit. Each of the five response integers is therefore bounded to 4300 decimal digits plus an optional sign and fixed JSON framing |
| Successful chat response | exactly five JSON integer keys, no model catalog fields | `CurrentChatConfig` and exact-body handler/integration assertions |
| Missing chat key | endpoint constant | exact defaults `10`, `150`, `150`, `10`, `3` |
| Present malformed chat value or unavailable vault | HTTP 500 | fail-closed tests; response is the current safe `{"message":"Internal Server Error"}` and never includes secret material |
| `GET /api/v2/elitea_core/project_context/prompt_lib/{projectID}/project-context` | requires `models.project_context.view` in default/prompt-lib mode | `CurrentRoutes`; same production authentication/RBAC chain |
| Project-context storage | only `p_<authorized project>.configuration`, `row.project_id = projectID`, `type = 'project_context'`, `LIMIT 1`, deliberately no `ORDER BY` and no public fallback | transaction-local `tenant.Executor`; real PostgreSQL test places a wrong-project canary in the selected tenant schema and proves it is ignored |
| Missing row | `{"id":null,"content":"","enabled":true,"updated_at":null}` | exact-body unit and PostgreSQL tests |
| Existing row | stored ID and naive timestamp, data defaults, Pydantic-compatible boolean coercion | parser and real PostgreSQL timestamp tests |
| Empty object data | defaults content to empty and enabled to true while retaining row ID/timestamp | parser matrix and persisted PostgreSQL `{}` fixture |
| Non-object data | upstream `ConfigurationDetails.data: dict` rejects SQL/JSON null, booleans, numbers, strings and lists before `ProjectContextDetail.from_config` | HTTP 500 parser fixtures and real PostgreSQL HTTP fixtures |
| Time budget | five seconds around the project-context storage operation | `CurrentProjectContextTimeout`; caller cancellation remains authoritative |
| Integer route converter | Werkzeug `<int:project_id>` accepts Unicode `Nd` digits and leading zeros | converter tests exercise ASCII, Arabic-Indic and Devanagari digits on both endpoints; non-`Nd` numerics and values outside PostgreSQL `int64` are rejected with 404 before auth or storage |
| Methods outside scope | project-context PUT/DELETE are not exposed | production-router tests require 405 when the GET route is composed |

Endpoint recommended-role comments are documentation only and never create
grants. Authorization is resolved from current PostgreSQL role bindings and
exact permission strings through `legacyrbac.PostgresResolver`; the real
database matrix explicitly covers project-specific permission overrides and
the current central role-name fallback.

## Evidence boundary

- Unit/component: parser golden vectors, defaults, precedence, exact JSON,
  route matching, auth-before-read, permission-before-read, failure redaction.
- Service integration: independently created PostgreSQL database, current
  encrypted Fernet vault tables, tenant schemas, current user and project RBAC,
  central role-name fallback, active forwarded user and PAT identities,
  suspension, tenant isolation, and persisted corrupt-data outcomes. The
  PostgreSQL CI job selects this test and the standalone central-fallback
  resolver test with required non-skipping environment gates.
- Concurrency/static: focused `go test -race` and `go vet` are required before
  integration.

This evidence does not claim a deployed browser checkpoint, penetration test,
load/soak result, or mutation parity.
