# Gateway: provider request bodies leave as compact JSON (2026-10-28)

## Why

Every Agent and ad hoc chat run in a project with an enabled project context failed its first model call. The Worker
reported `model_gateway.forbidden` ("the model gateway rejected the execution project"). The gateway logged
`llm_provider_refusal` with status 403, `gateway_origin:false` and an empty error type. Pipeline LLM nodes in the same
project, and runs in other projects with the same user, model and credential, answered 200.

Example: execution `0d6927e1cb972a242d6d1ddc24b203bb` on the `fanpar` stack, project 118, 2026-10-28T20:31:16Z,
`eu.anthropic.claude-haiku-4-5-20251001-v1:0` through the OpenAI-compatible credential `ai_creds` of project 1
(upstream `https://dev.elitea.ai/llm/v1`).

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
| LLM gateway × model call (before first token), run with an active instruction or markup in content | **F**: typed `model_gateway.forbidden`, every attempt, behind an edge that scores indented bodies | Not refused for this cause. Other refusals keep their typed failure (F). | `TestProviderBodiesAreCompact`; live before/after above |

No other rows change.

## Real-browser evidence

**Not collected. Blocked.** The rules forbid starting an image build or a stack below ~30 GiB free. When the fix was
ready, `/System/Volumes/Data` had 22 GiB free. The only stack with the project-context fixture is `fanpar`, which
belongs to another session and may not be redeployed without the user's approval.

What remains: build `elitea-llm-gateway` from this branch and run the §3b binary check. Then, on a stack with project
118's context enabled, run an Agent and an ad hoc chat ("what is the project verification color?"), confirm the
answer names LIME-953, and reload the conversation. The PR stays a draft until this is done.

## Fixtures

None created by this change. Project 118 and its project context were created by the session working on FU-246
(PR #1511). That context was `enabled:false` when this investigation read it, so the project-context run cannot be
repeated on `fanpar` as it stands. The replay used the frozen input bundle of the refused run.

## Follow-ups

- Upgrade bifrost/core to ≥ v1.8.0. It sends compact bodies on both paths, including extra-parameter merge. Then
  delete `internal/providerwire`.
- Readable refusal: the Worker maps every 403 to "the model gateway rejected the execution project" without reading
  the body. The gateway already returns "upstream provider returned an HTML error page (status 403)" for an edge page
  (`internal/llmproxy/httpio.go:238`). The Worker could read the bounded body as it does for 400 and stop blaming the
  project.
- The Worker-side content workarounds made for the 2026-10-19 HITL refusals (see that note) are no longer needed for
  this upstream. Keep or revert them deliberately.
