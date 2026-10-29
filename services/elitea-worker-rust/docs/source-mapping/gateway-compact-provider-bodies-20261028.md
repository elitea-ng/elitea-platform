# Gateway: provider request bodies leave as compact JSON (2026-10-28)

## Why

Every Agent and ad hoc chat run in a project with an enabled project context failed its first model call. The Worker
reported `model_gateway.forbidden` ("the model gateway rejected the execution project"). The gateway logged
`llm_provider_refusal` with status 403, `gateway_origin:false` and an empty error type. Pipeline LLM nodes in the same
project, and runs in other projects with the same user, model and credential, answered 200.

Example: execution `0d6927e1cb972a242d6d1ddc24b203bb` on the `fanpar` stack, project 118, 2026-10-28T20:31:16Z,
`eu.anthropic.claude-haiku-4-5-20251001-v1:0` through the OpenAI-compatible credential `ai_creds` of project 1
(upstream `https://dev.elitea.ai/llm/v1`).


> **Superseded (2026-10-29, #1520).** The gateway moved to bifrost/core v1.13.1, which builds every converted
> provider body with `MarshalProviderRequest` → `MarshalSorted` (compact), and no provider request path indents
> (`MergeExtraParamsIntoJSON` included). `internal/providerwire` and its guards are deleted, as planned below.
> `internal/server/provider_wire_test.go` (`TestProviderBodiesAreCompact`) now pins bifrost's body encoder to
> compact JSON with markup in a string, so a release that indents again fails CI. The record below describes
> the v1.7.15 fix.

## Root cause

Two things together cause the refusal. The first is ours.

1. **The gateway sends indented JSON.** bifrost/core v1.7.15 builds every converted provider body with
   `providers/utils.MarshalSortedIndent(body, "", "  ")` inside `CheckContextAndGetRequestBody`. The upstream
   receives a two-space indented body. The Worker sends compact JSON. The gateway re-encodes it.
2. **The edge in front of the upstream refuses that shape when a string carries markup.** The upstream sits behind
   Cloudflare. An indented body whose strings contain ordinary markup or comparison text is answered 403 with the HTML
   page "Cloudflare: WAF block". The same content sent compact passes.

The project context is only the trigger. With an enabled context and an empty activation description, Main freezes
the context as active. The Worker then puts it into the leading system text as
`<active_instruction id="project-context:118:2" revision="…">…</active_instruction>`
(`libs/rust/agent-runtime/src/instruction_authority.rs:412`, `render`). An active skill renders the same block.
Pipelines skip project context (`services/elitea-main/internal/application/agentexecution/instruction_snapshots.go:54`),
so their requests carried no markup. That is why they passed.

The `read_project_context` tool and its `{"type":"object","properties":{},"additionalProperties":false}` schema are
not involved.

### Captured request evidence

How the evidence was captured:

- The failing run's frozen input came from `elitea_runtime.input_bundle_entries` (bundle
  `055069d36904340be1b308b6b8498f86`): `instructions` empty, persona `cynical`, user turn `say "hi"`, and the project
  context snapshot.
- The Worker body was rebuilt in the shape `services/elitea-worker-rust/src/transport/openai_compatible_facade.rs`
  (`encode_request_body`) emits.
- That body was passed through the gateway's own conversion (`openai.OpenAIChatRequest` → `ToBifrostChatRequest` →
  `CheckContextAndGetRequestBody`) in a scratch harness (not committed). It printed the exact bytes the gateway sends:
  indented, with `<` and `>` escaped as `<` and `>`.
- Each variant was sent to the upstream with the stack's own credential. The credential was resolved in memory from
  the stack's vault and never printed.

| Body sent to the upstream | Result |
|---|---|
| Gateway bytes for the refused run (indented) | **403** `text/html`, "Cloudflare: WAF block" |
| Same, `<` replaced by a raw `<` | 403 WAF |
| Same, without the `tools` array | 403 WAF |
| Same content, compact (`json.Compact` of the gateway bytes) | **200** stream |
| Same content, compact, with `<` and `>` as `<` and `>` | 200 |
| Indented, system text without the `<active_instruction>` block | 200 |
| Indented, tool with empty `properties` alone | 200 |
| Indented, user turn `Fix this: <div class="x">hi</div>` | 403 WAF |
| Compact, same user turn | 200 |
| Indented, system text `if a < b and c > d then stop` | 403 WAF |
| Compact, same | 200 |
| Indented, user turn with `SELECT … OR 1=1; <script>alert(1)</script>` | 403 WAF |
| Compact, same | 200 |
| `indent=0` (newlines, no indentation), active-instruction block | 200 |

The edge scores content, so content changes are guesswork:

- `<web_content nonce=…>…</web_content>` passed indented;
- `a < b and c > d` did not.

Indentation is the one deterministic factor. Every body that was refused indented passed compact, including the
script-and-SQL probe.

The 2026-10-19 note ([gateway provider refusal log](gateway-provider-refusal-log-20261019.md)) saw the same edge refuse
"combinations of ordinary content" after a HITL decision. Those bodies were indented too, so the same cause explains
them.

## Business behaviour

No change in what a caller sends or receives. The upstream now receives the same JSON value with no insignificant
whitespace: same keys, same values, same key order. Current platform reference: LiteLLM sends compact JSON
(`httpx`/`json.dumps`). Nothing is ported. The indentation was a bifrost artefact.

bifrost/core v1.8.0 and later send compact bodies themselves (`MarshalProviderRequest`). This change gives v1.7.15 the
same wire shape without a 358-commit dependency upgrade.

## Changed paths

- `services/elitea-llm-gateway/internal/providerwire/providerwire.go` (new):
  - links bifrost's unexported provider-body encoder `providers/utils.sortedAPI` (`:65`);
  - replaces it once, at package init (`:80-91`), with `compactAPI`. `compactAPI` keeps `SortMapKeys` and answers
    `MarshalIndent` with `Marshal` (`:73`).
  - `init` refuses to start on a nil link target (`:81`). It also refuses to start when bifrost's own
    `MarshalSortedIndent` still answers indented after the swap (`:86`).
- `services/elitea-llm-gateway/internal/server/server.go:28`: the package that initialises bifrost core imports
  `providerwire`, so the gateway binary always carries it.
- `services/elitea-llm-gateway/go.mod`: `github.com/bytedance/sonic` is now a direct requirement. Same version. `go mod
  tidy` produces the same file.
- Tests:
  - `internal/providerwire/providerwire_test.go` (new);
  - `internal/server/provider_wire_test.go` (new).

## Tests

`GOWORK=off go test -race -count=1 ./...` in `services/elitea-llm-gateway`: 20 packages, all `ok`, no new skips (4 new tests).
`golangci-lint run ./...` at the CI-pinned v2.9.0: `0 issues`.

| Test | Proves | Without the fix |
|---|---|---|
| `TestProviderBodiesAreCompact` | The refused run's request goes through real bifrost core and the vLLM provider to a fake upstream. The upstream body equals its `json.Compact` form. The active-instruction text and the tool declaration are intact. | Fails: "upstream body carries insignificant whitespace (928 bytes, 720 compact)" |
| `TestMarshalSortedIndentIsCompactAndSorted` | The replaced encoder sorts keys and adds no whitespace. | Fails: indented output |
| `TestServerLinksCompactProviderBodies` | `internal/server`, which initialises bifrost core, carries the override. Dropping the import fails it. | Fails |
| `TestBifrostVersionIsTheVerifiedOne` | `go.mod` requires bifrost/core `v1.7.15`, the release the link target was verified against. Any bump fails until it is re-verified, or the package is deleted for v1.8.0 or later. | n/a (guard) |

Fail-closed check: with the link target renamed to a symbol that does not exist, the `internal/server` tests stop at
start-up with `panic: providerwire: bifrost providers/utils.sortedAPI is not linked; re-verify against bifrost/core
v1.7.15`. That run was made, then reverted.

### Live proof against the real upstream

A scratch program (not committed, built at `ad19a0da0`) ran real bifrost core with the vLLM provider against
`https://dev.elitea.ai/llm`. It
used the stack's credential and sent the refused run's request. It was built twice: as main is today, and with
`providerwire` linked.

```
before  REFUSED status=403 message="HTML response received from provider"
after   OK answered 3 chars: "Hi!"
before  REFUSED status=403 message="HTML response received from provider"
after   OK answered 3 chars: "Hi!"
```

## Performance

- Mechanism: `providerwire.go:73`. One sonic `Marshal` replaces `MarshalIndent`. Sonic's indent pass is skipped.
- Measured:
  - the refused run's body went from 928 to 720 bytes (−22 %, `TestProviderBodiesAreCompact` output without the fix);
  - a 40-tool request with a 2 KB system text went from 44,017 to 19,822 bytes (−55 %).
- Effect: fewer bytes per model call, both on the wire and in any request-size limit at the upstream.

## Durability

Not affected. The change is wire formatting only. No state, checkpoint or identity is involved.

## Resilience

- An edge refusal that the indented shape triggered can no longer happen on this path.
- A linkname checks neither that the target exists nor its type. A missing target still builds and leaves the local
  variable nil. Three guards make the seam fail closed:
  - `TestBifrostVersionIsTheVerifiedOne` blocks any bifrost bump until the target is re-verified;
  - `init` refuses a nil target;
  - `init` probes the encoder bifrost really uses and refuses an indented answer.

  The gateway therefore cannot start sending indented bodies again unnoticed, and cannot start on a target it has not
  checked.
- `init` runs before any provider can dispatch, after `providers/utils` initialised the variable, because of the
  import order.
- Known gap: `providers/utils.MergeExtraParamsIntoJSON` re-indents with `encoding/json` when a caller sends fields
  bifrost does not model while extra-parameter passthrough is on. The Worker sends no such fields. Other callers that
  add free-form extras can still produce an indented body. bifrost ≥ v1.8.0 closes it (follow-up).

## Security

| Category (`rules/security.md`) | Applies | How it was checked |
|---|---|---|
| Trust boundaries and identity | No | Headers, identity and the hop marker are untouched. |
| Authorization | No | No route or decision changed. |
| Input, parsing and amplification | No | The same decoded value is re-encoded. The size can only shrink. |
| Injection and construction | No | No string building. The encoder is sonic's, with key sorting. |
| Egress and SSRF | No | Destinations, dialer, private-network policy and the error sanitiser are unchanged (`internal/llmproxy/httpio.go:238`). |
| Secrets | Yes (evidence only) | The replay resolved the stack's credential in process and printed only lengths and statuses. Diff secret-scanned. No credential in code, tests or this document. |
| Supply chain | Yes | No version change. `sonic` moved from indirect to direct. `govulncheck ./...`: 1 standard-library finding, GO-2026-6809 (`crypto/x509@go1.26.9`, fixed in go1.26.10). It is the toolchain, not this diff, and it is pre-existing. 0 findings in imported packages. |

The linkname reaches into a third-party package's unexported variable. It is a deliberate, documented seam that lasts
until the bifrost upgrade. It writes one interface value, of the type verified for v1.7.15. A version pin and start-up
probes guard it (Resilience).

## Recovery guarantee rows

| Component × phase | Before | After | Evidence |
|---|---|---|---|
| LLM gateway × model call (before first token), run with an active instruction or markup in content | **F**: typed `model_gateway.forbidden`, every attempt, behind an edge that scores indented bodies | Not refused for this cause. Other refusals keep their typed failure (F). | `TestProviderBodiesAreCompact`; live before/after; browser A/B on `ctx403` (executions `3077849a…` → `0d8e2e74…`, `29949835…`) |

No other rows change.

## Real-browser evidence

Collected 2026-10-29 on my own stack `ctx403` (`http://ctx403.localhost:19400`, Chrome, mock OIDC subject
`admin@centry.user`). No response mocks. Real model `eu.anthropic.claude-haiku-4-5-20251001-v1:0` through the
OpenAI-compatible credential of project 1 (`https://dev.elitea.ai/llm/v1`).

The stack was brought up with `production-profile.pre-defaults-20261019/stack.sh up` (real-model dump
`product-real-models-main-d5e966074.dump`). `verify-profile` passed 50 of 50, both before and after the gateway swap.

Images:

| Service | Image |
|---|---|
| Main | `elitea-main:2a2-be5a8ed4c` |
| Worker | `elitea-worker-rust:2a2-be5a8ed4c` |
| Web | `elitea-web:2a2-be5a8ed4c` |
| Supervisor | `elitea-sandbox-supervisor:2a2-40178539e` |
| Runners | `elitea-code-runner:ctx403-be5a8ed4c-{deno,rust}` (built from `be5a8ed4c`, which contains #1160) |
| Gateway, control | `elitea-llm-gateway:main-d5e966074-verify` (`108750a4…`) |
| Gateway, this branch | `elitea-llm-gateway:ctx403-a7dd93bcc` (`983856f9…`) |

These are the same Main, Worker, Web and Supervisor images as the stack where the refusal was first seen. Only the
gateway changes between the two runs, so the A/B isolates this change.

§3b binary check (`stack.sh binary-check <image> "providerwire: bifrost provider bodies are still indented" --path
/elitea-llm-gateway`):

- `ctx403-a7dd93bcc`: count=1, OK;
- `main-d5e966074-verify`: count=0 (control).

Fixture: Settings → Project Context in project 2 (Private), created through the UI. It is enabled, with no activation
description, and its content is "The revision verification color is LIME-953. Always include this exact code when
asked for the project verification color." The saved Agent "Context color agent ctx403" (id 170, version 196) was
created through the UI with the model set to Haiku.

| Run | Gateway | Execution | Result in the browser | Logs |
|---|---|---|---|---|
| Ad hoc chat 887, "What is the project verification color?" | control | `3077849a49f1a302cd210c9458475dc8` (FAILED) | "The model service denied access. Ask an administrator to check the model credentials and project permissions." | Gateway `llm_provider_refusal` status 403, `gateway_origin:false`. Worker `model_gateway.forbidden`, `tool_count=1`. |
| Ad hoc chat 888, same question | this branch | `0d8e2e74d861d5db3181f8f448a36579` (SUCCEEDED) | "The project verification color is LIME-953." | 2 model calls, `tool_count=1`, 0 refusals |
| Saved Agent 170, chat 890, same question | this branch | `299498353c64a990114c1913f8aa7664` (SUCCEEDED) | The model called `read_project_context` (tool card with the frozen content), then answered "The project verification color is LIME-953." | 2 model calls, `tool_count=1`, 0 refusals |

After a reload:
- chats 888 and 890 still show the LIME-953 answers;
- chat 887 still shows the refusal.

The stack was torn down after recording (`stack.sh down --name ctx403 --volumes`).

## Fixtures

- Bisect and live proof: the frozen input bundle of the refused `fanpar` run (project 118, created by the FU-246
  session). Nothing was changed on `fanpar`.
- Browser proof: the project context and Agent 170 on my own stack were created through the UI, as described above.

## Follow-ups

- Upgrade bifrost/core to ≥ v1.8.0. It sends compact bodies on both paths, including extra-parameter merge. Then
  delete `internal/providerwire`.
- Readable refusal: the Worker maps every 403 to "the model gateway rejected the execution project" without reading
  the body. The gateway already returns "upstream provider returned an HTML error page (status 403)" for an edge page
  (`internal/llmproxy/httpio.go:238`). The Worker could read the bounded body as it does for 400 and stop blaming the
  project.
- Project-context parity with the current platform:
  - With no activation description, the current platform puts the context straight into the instructions under a
    `# Project Context` markdown heading and offers no tool
    (the legacy `elitea_core` plugin, `utils/project_context_utils.py:8-22`).
  - Only with an activation description does it withhold the content and bind `read_project_context`
    (`elitea_sdk/runtime/middleware/project_context.py`).
  - The Worker renders an `<active_instruction …>` block and still exposes `read_project_context` when the context is
    already active (`libs/rust/agent-runtime/src/instruction_authority.rs:234`, `toolsets`).
  - This is not the cause of the refusal: the current platform sends compact JSON through LiteLLM. It is a
    behavioural difference to settle separately.
- The Worker-side content workarounds made for the 2026-10-19 HITL refusals (see that note) are no longer needed for
  this upstream. Keep or revert them deliberately.
