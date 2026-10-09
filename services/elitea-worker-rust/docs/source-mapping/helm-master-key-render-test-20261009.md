# Helm master-key render test after the network-policy guard (2026-10-09)

## Defect

On `main` at `1ab920dde`, `deploy/helm/tests/render-main-master-key.sh` failed
4 of its 9 checks: the default render, an explicit key reference, the
development opt-out, and no key source plus the opt-out. Every one stopped at
`deploy/helm/elitea/templates/guards.yaml:29-31`. That guard refuses any render
with `networkPolicies.enabled=true` (the default) that does not say who may
reach `elitea-main:8080`, and the script set neither
`networkPolicies.main.noExternalIngress` nor `ingressFrom`. So none of the
master-key logic was exercised.

Cause: a merge-order gap. #1176 (`b0d43a278`) added the guard and merged first.
#1175 (`602694807`) added the script on a branch that did not have the guard,
so the script passed there and failed only after the merge. No workflow and no
task ran the script (`helm-lint.yml` and `Taskfile.yml` never named it), so
nothing reported it.

## Business behaviour

Not applicable: this changes a render test and its local runner, not runtime
behaviour. Nothing is ported from the current platform. The chart behaviour
under test (`elitea-main.masterKeyRef`, `templates/main/_helpers.tpl:1276-1295`;
`templates/main/deployment.yaml:19-21`) is unchanged and is recorded in
[access-hardening-20261008.md](access-hardening-20261008.md).

## Changed paths

| Path | Change |
| --- | --- |
| `deploy/helm/tests/render-main-master-key.sh` | Every render declares `networkPolicies.main.noExternalIngress=true`. Of the other 18 shell render tests, 16 use the same flag, and `render-edge-healthz.sh` and `render-platform-edge-identity.sh` list `ingressFrom` instead. `refuses` now fails a refusal that came from a network-policy guard instead of the master-key guard. The "no source" and "no Secret name" refusals match their own guard's phrase (`has no SECRETS_MASTER_KEY source`, `needs both secretName and key`). The old patterns also matched the other guard's message. |
| `Taskfile.yml` | New `helm:main-master-key` task, run by `helm:lint` next to `helm:capabilities`, `helm:llmpath`, `helm:edge-healthz`, `helm:bf02b` and `helm:compiled-snapshots`. `.github/workflows` is unchanged. |

`noExternalIngress=true` touches only the NetworkPolicy on elitea-main, never
its env. The guard that demands the decision stays asserted by
`render-network-policies.sh:279`.

## Tests

Local: helm v4.1.0, yq v4.53.6, Python 3.12.10, macOS arm64, `1ab920dde` plus this change.

| Run | Result |
| --- | --- |
| `render-main-master-key.sh` before | 5/9 ok, 4 FAIL at `guards.yaml:30` |
| `render-main-master-key.sh` after | 9/9 ok |
| All 19 `deploy/helm/tests/render-*.sh` + `render-compiled-snapshots.py` after | 20/20 pass |
| `helm lint` on `deploy/helm/{elitea,nats,nats-bootstrap}` (the `helm:lint` loop) | 3/3, 0 failed; only `[INFO] icon is recommended` |
| `helm:code-consumers` (3 Python scripts) | 3/3 OK |

Skips:
- **kubeconform**: `render-nats-security.sh` skipped kubeconform validation of the NATS renders, because it is not installed locally. CI sets `NATS_REQUIRE_KUBECONFORM=1`.
- **`task` binary**: not installed locally, so each command in `helm:lint` was run directly.
- **NATS subchart**: `render-nats-security.sh` needs it vendored. That ran with `helm repo add` in a throwaway `HELM_*` home (pinned `nats` 1.3.9). Before that, the script failed locally with "no repository definition", which was an environment issue, not a chart issue.

### Mutation proof that the assertions still bite

Each mutation was applied to a scratch copy of `deploy/helm/{elitea,tests}`: to the chart templates (`main/_helpers.tpl`, plus `main/deployment.yaml` for the overwrite row) or, in the last row, to the script. The fixed script was then run against the copy:

| Mutation | Caught by |
| --- | --- |
| reference always `optional: true` | "default reference is optional" |
| reference never optional | "with the development opt-out the reference is still required" |
| "no source" guard removed | "the chart rendered no master key source at all" |
| `main.env.SECRETS_MASTER_KEY` guard removed | "the chart rendered a plaintext SECRETS_MASTER_KEY" |
| empty `secretName` accepted | "the chart rendered a master key reference with no Secret name" |
| gateway reference overwrites an explicit `main.secrets` entry | "an explicit main.secrets.SECRETS_MASTER_KEY did not win" (+ the empty-name refusal) |
| empty `secretName` falls through to the no-source guard (`if and $ref $ref.secretName`) | "message does not name 'needs both secretName and key'". The old `main\.secrets\.SECRETS_MASTER_KEY` pattern passed this one, which is why it was tightened |
| script without `noExternalIngress=true` | reproduces the original 4 failures |

In the script mutation, the three refusals still pass, because helm reaches the
master-key guard before `guards.yaml`. The new network-policy branch in
`refuses` keeps a later template-order change from turning those refusals into
false passes.

## Performance / Durability / Resilience / Security

| Property | Mechanism | Proof |
| --- | --- | --- |
| Performance | n/a. One render test, about 9 `helm template` calls, local only. | — |
| Durability | n/a. No runtime state. | — |
| Resilience | The test now reaches the logic it names, and it cannot pass on an unrelated refusal (`render-main-master-key.sh`, `refuses`). | Mutation table above |
| Security | Restores the proof of fail-closed startup config for the vault master key: a missing key source refuses to render, a plaintext key is refused, the default reference is required, and the opt-out only makes it optional. No secret values; the plaintext probe is the existing all-`A` dummy. | 9/9 ok + mutation table |

## Recovery guarantee rows

No component × phase changes: the change is test-only, with no runtime path.
The existing row "Main × start-up (vault key)", class **F**
([access-hardening-20261008.md:150](access-hardening-20261008.md)), is
unchanged. This script is again the proof of its render-time half: the chart
refuses a values set that would start Main with no key source.

## Browser evidence

Not applicable. Nothing rendered or served changes, so there is nothing to
confirm in a browser. Evidence is the render output above.

## Fixtures

None. Every case is a `helm template` of the in-repo chart with `--set` overrides.

## Follow-ups

- `.github/workflows/helm-lint.yml` still does not run `render-main-master-key.sh`, `render-network-policies.sh`, `render-platform-edge-identity.sh` or `render-sandbox-images.sh`. Adding them needs a workflow change, which is out of scope here.
- `Taskfile.yml` `helm:lint` runs only 6 of the 20 render tests, so `task helm:lint` is not a full local equivalent of the CI job.
- The render scripts each hard-code their posture flags. A shared render-only posture under `deploy/helm/tests/` would make the next chart-wide guard a one-place change.
- `scripts/lib/assertion-floor.sh` (used by `render-bf0-2b.sh`) could give this script a derived floor on the number of checks that ran.
- This defect came from two PRs that were each green alone. Re-running every render test after merging `main` would catch the next one.
