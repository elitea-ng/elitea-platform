# LLM gateway: mandatory vault master key and its own routing CEL cap (2026-10-09)

Follow-ups F13 and F14 of the point 5 wave 1 plan (package D). F13 hardens the
gateway's vault key posture (local findings R-5, H-06). F14 gives the gateway's
load-time routing compiler its own length bound.

## Business behaviour

Nothing is ported from the current platform. The behaviour mirrored is
elitea-main's own, from #1175 and #1184:

- `services/elitea-main/cmd/elitea-main/master_key_gate.go` refuses to start
  without `SECRETS_MASTER_KEY`, unless `ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS=true`;
  it always refuses a malformed key.
- `deploy/helm/elitea/templates/main/_helpers.tpl` (`elitea-main.masterKeyRef`)
  makes the chart refuse a values set with no key source, and
  `render-main-master-key.sh` proves it.
- `services/elitea-main/internal/api/gateway/routing_cel.go:141`
  (`maxRoutingCELBytes = 8 << 10`) refuses a longer routing predicate at
  authoring time.

The gateway now applies the same rules with the same variable names and the same
constant value. A local stack sets the one opt-out variable for both services.

Deliberately not ported:

- elitea-main's `refuseUnwrappedVaultKeys` row check. The gateway only reads the
  vault, and elitea-main, which shares the database, already refuses to start
  over unwrapped rows when a key is set.
- An `optional` field on the gateway's chart entry no longer has any effect. The
  opt-out alone decides, as for elitea-main.

## Changed paths

| Path | Change |
| --- | --- |
| `services/elitea-llm-gateway/internal/account/vault.go:48-90` | `MasterKeyEnvVar`, `AllowUnwrappedEnvVar`, and `MasterKeyFromEnv(getenv)` (validates; the error names the variable and fault, never the value). `NewFernetVault` uses it. |
| `services/elitea-llm-gateway/cmd/elitea-llm-gateway/master_key_gate.go:26` | `requireVaultMasterKey`: valid key → start; malformed → refuse even with the opt-out; absent → refuse unless the opt-out is `true`; any other opt-out value → refuse. |
| `services/elitea-llm-gateway/cmd/elitea-llm-gateway/main.go:69` | The gate runs before the database pool opens; `FATAL: refusing to start` + exit 1, or a loud warning under the opt-out. |
| `services/elitea-llm-gateway/internal/policy/routing.go:28,111` | `maxRoutingCELBytes = 8 << 10` and `ErrRoutingCELTooLong`, checked in `CompileCEL` before the parser. Load path: `compile.go:174` → `parseRoutingRule` → `CompileCEL`; an over-cap row is recorded in `Snapshot.Rejected` and the other rules load. |
| `deploy/helm/elitea/templates/llmGateway/_helpers.tpl:154` | `elitea-llm-gateway.masterKeyRef` refuses five things: `llmGateway.env.SECRETS_MASTER_KEY`; an opt-out value the binary would refuse (anything but `true`/`false`); an entry that is not a map; a reference without `secretName`/`key`; no reference without the opt-out. It emits `optional` = opt-out. |
| `deploy/helm/elitea/templates/llmGateway/deployment.yaml:41-51` | The checked reference replaces the entry, or the entry is dropped (no source + opt-out). |
| `deploy/helm/elitea/values.yaml:2741` | The `optional: true` decision is reversed and the comment rewritten. |
| `deploy/helm/elitea/templates/main/_helpers.tpl` | Comment only: each service has its own opt-out. |
| `deploy/helm/tests/render-gateway-master-key.sh` | New render test, 16 checks. |
| `services/elitea-llm-gateway/scripts/env-drift-allowlist.txt` | The opt-out variable is allowlisted with its justification: it is absent from values on purpose. |
| `Taskfile.yml:258,269` | `helm:gateway-master-key`, run by `helm:lint` next to `helm:main-master-key`. `.github/workflows` is unchanged. |
| `deploy/docker-compose.standalone-full.yml:496,521` | The gateway gets `ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS: "${…-true}"`, as elitea-main already has (`:891`). A supplied key is still used. |
| `deploy/README.md`, `services/elitea-llm-gateway/DECISIONS.md` | The master-key section and the open decision now describe the gateway too. |

Other compose files and the kind values need no change: `docker-compose.yml`,
`docker-compose.staging.yml` and `docker-compose.e2e-standalone.yml` run no
gateway, and `deploy/kind/values-kind.yaml` sets `llmGateway.enabled: false`.

## Tests

Go 1.26.9 in `golang:1.26.9-bookworm` (the host has 1.26.5), `GOWORK=off`.
helm v4.1.0 and yq on macOS arm64.

| Test | What it proves |
| --- | --- |
| `master_key_gate_test.go:25` `TestRequireVaultMasterKey` (10 cases) | Absent, empty, opt-out `false` → refused and the error names both variables. Opt-out `true` → warning. Valid key, with or without the opt-out → no warning. Malformed, trailing space with opt-out, wrong length → refused, and the error does not contain the key material. Opt-out `yes` → refused. |
| `master_key_gate_test.go:100` `TestGatewayRefusesToStartWithoutAMasterKey` | Re-executes the test binary as the real `main()` with no database pool. No key → non-zero exit, output names `FATAL: refusing to start`, `SECRETS_MASTER_KEY` and the opt-out. Negative controls in the SAME environment: a valid key, and the opt-out, both keep serving; the opt-out logs `UNWRAPPED`. |
| `main_test.go:53` `TestMainWiring` | `requireVaultMasterKey(` is a call expression in package main. |
| `routing_test.go:280` `TestCompileCELLengthBound` | Exactly 8192 bytes compiles; 8193 → `errors.Is(err, ErrRoutingCELTooLong)`; the error does not echo the expression. |
| `routing_test.go` `TestRoutingCELCapMatchesMain` | Reads both sources and fails if this `maxRoutingCELBytes` and elitea-main's differ. |
| `routing_test.go:303` `TestHandWrittenOverlongRuleIsRejectedAtLoad` | Through `Compile`: the 8192-byte rule loads and the 8193-byte rule is rejected, named, with the cap in the reason and no expression echo. |
| `render-gateway-master-key.sh` (16 checks) | The default reference is `optional: false`, rendered once and never as a plain value. A stale `optional: true` is ignored, and an operator-named Secret is honoured. The opt-out gives `optional: true` and the variable reaches the container; an explicit `false` keeps it required. Refused: a plaintext key, an empty `secretName`, an empty `key`, a non-map entry, an opt-out of `True`, and no source. No source plus the opt-out renders without a reference. |

`metrics_test.go` and `budget_startup_gate_test.go` start the real process; they
now pass a generated test key (`testMasterKey`, `master_key_gate_test.go:23`),
so they still fail only on what they test.

| Run | Result |
| --- | --- |
| `go test ./cmd/... ./internal/account/ ./internal/policy/` | ok (3 packages) |
| `go test -race ./...` (gateway module, after review fixes) | 19 packages ok, 0 FAIL |
| `golangci-lint run ./...` (v2.14.0, CI uses `latest`) | 0 issues |
| `scripts/env-drift-check.sh` | 0 fail. The gateway has 1 warn (`GATEWAY_HTTP_ADDR`, which predates this change). elitea-main's warns predate it too. |
| `go test ./...` (gateway module) | 19 packages ok, 1 without tests, 0 FAIL. In the three touched packages: 342 tests and subtests passed, 2 skipped (`TestPostgresBudgetProbe*`, which need a PostgreSQL URL and do not touch this change) |
| `go vet` on the touched packages, `gofmt -l` | clean |
| `render-gateway-master-key.sh` | 16/16 ok |
| All 21 `deploy/helm/tests/render-*.sh` + `render-compiled-snapshots.py` | 22/22 pass |
| `helm lint` on `deploy/helm/{elitea,nats,nats-bootstrap}` | 3/3 pass |
| `docker compose -f deploy/docker-compose.standalone-full.yml config` | valid; the opt-out resolves to `true` for both services |

Skips:
- **`task` binary**: not installed locally, so each `helm:lint` command was run directly.
- **NATS subchart**: `render-nats-security.sh` needs it vendored. It ran with `helm repo add` in a throwaway `HELM_*` home. Without that it fails with "no repository definition", an environment issue.
- **kubeconform**: not installed locally, so `render-nats-security.sh` skipped it. CI sets `NATS_REQUIRE_KUBECONFORM=1`.

### Mutation proof

| Mutation | Caught by |
| --- | --- |
| `CompileCEL` cap check disabled | both routing tests FAIL |
| `main.go` calls a stub instead of `requireVaultMasterKey` | `TestMainWiring` FAIL; process test: "absent key refuses" FAIL (the process kept running), opt-out control FAIL (no `UNWRAPPED` warning) |
| chart templates and `values.yaml` restored to `origin/main` | `render-gateway-master-key.sh` 6 FAIL: default optional, stale optional, and all four refusals |

## Performance / Durability / Resilience / Security

| Property | Mechanism (`path:line`) | Proof | Measured |
| --- | --- | --- | --- |
| Performance | `routing.go:111` rejects an over-cap predicate by length, before parsing. Every replica compiles every rule on every policy load (`compile.go:174`), so the cap bounds that work per rule. The key gate is one env read at start. | `TestCompileCELLengthBound`, `TestHandWrittenOverlongRuleIsRejectedAtLoad` | The policy package suite runs in under 0.1 s, including an 8 KiB compile. |
| Durability | No new state. A rejected routing row stays in the table and is reported on `/governance/status` via `Snapshot.Rejected`; the remaining rules load. | `TestHandWrittenOverlongRuleIsRejectedAtLoad` | — |
| Resilience | Named limit `maxRoutingCELBytes` with a typed `ErrRoutingCELTooLong`; limit and limit+1 tests. The start-up refusal is typed, names the variable and the remedy, and fires before any dependency is opened (`main.go:69`). | gate unit + process tests; routing limit tests | Process refusal is immediate; the controls run 3 s each. |
| Security | Fail-closed start-up for a required secret, at the binary (`master_key_gate.go:26`) and at render time (`_helpers.tpl:154`). The key is a Secret reference, never a plain value. No error or log line carries key material or the CEL expression. | 10 gate cases incl. no-leak; process test; 13 render checks; mutation table | — |

Security categories (`rules/security.md`):

- **Trust boundaries / identity, authorization:** not touched.
- **Input bounds:** applies. The CEL cap is checked before parsing, and the
  limit and limit+1 tests above prove it.
- **Injection and construction:** not touched. No SQL, template or URL is built.
- **Egress:** not touched.
- **Secrets:** applies. A required secret is mandatory at start-up and fails
  closed; the key is by reference only; errors do not leak it. The diff was
  secret-scanned before each commit; the only key-shaped values are the
  documented all-`A` render probe and a generated test key.
- **Supply chain:** go.mod/go.sum are unchanged. `govulncheck` v1.1.4 on the
  gateway module: 0 called, 0 imported; 1 advisory in a required module that
  the code does not reach. That one is pre-existing.

## Recovery guarantee rows

| Component × phase | Class | Evidence |
| --- | --- | --- |
| LLM gateway × start-up (vault key) | F | A typed refusal names the variable and the remedy before the pool opens. Nothing is served without a key outside the documented opt-out. `master_key_gate.go:26`, `TestGatewayRefusesToStartWithoutAMasterKey`. Recovery is operator action (supply the Secret); no request work exists yet to lose. |
| LLM gateway × admission (policy load, routing rule) | F | An over-cap row is rejected per row with a typed reason in `Snapshot.Rejected`; every other definition loads and is enforced. A rule that cannot be compiled cannot be resumed or retried. `routing.go:111`, `TestHandWrittenOverlongRuleIsRejectedAtLoad`. |

No touched row is L. The existing "Main × start-up (vault key)" row
([access-hardening-20261008.md](access-hardening-20261008.md)) is unchanged.

## Real-browser and image evidence

No browser evidence: nothing rendered or served by the Web changes, and the
gateway's behaviour on a running stack is either "starts as before" (a key is
supplied) or "does not start". The process-level proof is
`TestGatewayRefusesToStartWithoutAMasterKey`, which runs the real `main()`.

No stack image was built. Free disk was 21 GiB, below the ~30 GiB floor for a
build. No shared stack was touched.

The three running local stacks (`elitea-verify-0def77b2`, `elitea-respipe` and
`elitea-dts`, all from `docker-compose.standalone-full.yml`) already pass a
`SECRETS_MASTER_KEY` to their gateway containers. This was read from
`docker inspect`; the value was not printed. A gateway image from this branch
would start unchanged on all three.

## Fixtures

None. The Go tests use injected env maps and a generated 32-byte test key. The
render test uses `helm template` with `--set` overrides of the in-repo chart.

## Review

`code-review` (high) on the full diff reported 7 findings.

Fixed:
- `-race` and golangci-lint not yet run.
- The env-drift warning for the opt-out.
- The chart not validating the opt-out value.
- A non-map entry giving an opaque error.
- The CEL cap duplicated with no parity check.
- A 60 s wait when the gate regresses (now 15 s).

Kept as a follow-up: rules per row and targets per rule are still unbounded at
load (below).

## Notes

- **F15 (ops, not code).** The old rehearsal database records the execution
  interrupts ledger migration as 155, while Main's own 0155 is
  `local_turn_executions`. Main from current `main` will not start on that
  database. The stack that used it, `elitea-nats-candidate`, is stopped and
  superseded by the verify stack. Before anyone revives it, restore its
  snapshot or renumber the row. Nothing in this PR touches migrations.

## Follow-ups

- The gateway's load path bounds each predicate, but not the number of rules in
  one row's `routing.rules` array or the number of targets per rule. elitea-main
  bounds both only through its 256 KiB request body. A hand-written row can
  still carry many 8 KiB rules.
- `.github/workflows/helm-lint.yml` does not run `render-gateway-master-key.sh`,
  which is the same gap noted for `render-main-master-key.sh`. Adding it needs a
  workflow change, which is out of scope here.
- Image-level evidence (extract the binary, confirm the branch string, run it
  keyless) is pending free disk.
