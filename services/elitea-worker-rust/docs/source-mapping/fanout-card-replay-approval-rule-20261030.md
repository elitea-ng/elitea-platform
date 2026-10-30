# A parked fan-out card survives the approval-rule upgrade (FU-268), 2026-10-30

- **Branch:** `fix/fu268-card-replay-approval-rule`, off `main` e7842b62f, which contains #1540 and #1541.
- **Source:** REVIEW-1540-overlap B1, queued by the orchestrator with Roman's approval.
- **Scope:** Worker code only; Main gets a test.

## Business behaviour

- **The run.** A fixed Parallel run is parked on a pipeline sensitive-tool card raised by a Worker from before #1540.
- **The upgrade.** Main and the Workers are upgraded, and a card is decided.
- **The defect.**
  - The wake raises every open card again (`libs/rust/agent-runtime/src/graph/fanout/runner.rs:421-428`).
  - #1540's projection always added `approval_rule` to the card's private pending object, so the re-raised card had a
    new `payload_sha256`.
  - Main accepts a re-raise only when the bytes are equal, or differ only by an added `display.argument_digest`
    (`sameRaisedCard`, `services/elitea-main/internal/infra/db/repos/execution_interrupts.go:788`). So it refused the
    card, and the run stopped instead of resuming.
- **The fix.** The card carries `approval_rule` only when the stored pause recorded one, as the pause itself does
  (`llm::with_policy_approval_rule`).
  - A card from an earlier pause is raised again with its earlier bytes.
  - A policy rule, or any recorded rule, stays part of the card.

## Changed paths

- `src/agents/events.rs`, in `fanout_card_parts`, the sensitive-tool branch: `approval_rule` goes into `pending` only
  when `data.approval_rule` is `Some`.
- Not changed: the browser's `hitl_interrupt` metadata (`events.rs` around `:2516`). It still shows the platform rule
  for a pause without one. Main does not hash it as card bytes.

## Tests

- **Fixtures,** made by running the Workers' own code (not written by hand):
  - `tests/fixtures/fu268/tool-guard-pause-pre-1540.json`: the pause a pre-#1540 Worker (`0990ab10f`) stored for a
    pipeline sensitive tool in a fixed Parallel member.
  - `services/elitea-main/internal/infra/db/repos/testdata/fu268/tool-guard-card-pre-1540.json`: the card that Worker
    raised for that pause.
  - `testdata/fu268/tool-guard-card-1540.json`: the card #1540's Worker (`e7842b62f`) projects from that stored pause.
  - `testdata/fu268/tool-guard-card-fu268.json`: the card this branch projects from that stored pause.
- **Worker:** `a_tool_guard_card_from_an_earlier_pause_keeps_its_bytes_and_a_recorded_rule_is_kept` (in
  `src/agents/graph/parallel_cards_tests.rs`).
  - Projecting the stored pause gives the earlier card, except for the tolerated `display.argument_digest`, with the
    same `payload_sha256` and `interrupt_id`.
  - It equals the `fu268` fixture.
  - Adding a policy rule, or an explicit platform rule, to the pause changes the card's digest.
  - It fails with #1540's projection ("the earlier Worker's card").
- **Main, real PostgreSQL:** `TestRaiseReplaysAToolGuardCardRaisedBeforeTheApprovalRuleRelease`
  (`services/elitea-main/internal/infra/db/repos/execution_interrupts_upgrade_replay_postgres_integration_test.go`).
  - It raises the pre-#1540 card.
  - #1540's card is refused with `ErrRaiseConflict`.
  - This branch's card is a replay of the same key, and the row stays PENDING revision 1.
  - The card is then decided once.
- **Measured:** the stored pause's earlier digest is `532a1f3d…`. #1540 gives `c0b3e159…`; this branch gives
  `532a1f3d…`.

## Performance

- No change: one conditional map insert per card projection.

## Durability

- The card identity contract holds across this upgrade: a restore or a wake re-derives the same card bytes, so a
  decision survives both (fanout-interrupt-decisions-v1 §3.1).

## Resilience

- A parked run that spans the upgrade resumes instead of stopping with `FrameUndelivered`.
- Cards raised after the upgrade with a policy rule carry it, exactly as before this fix.

## Security

- The approval rule is still bound into the card bytes whenever the pause recorded one, so a rule cannot be stripped
  from a card that had it.
- An absent rule means the platform rule, as the pause data has always read it.
- No new input, route, query, egress or credential path.
- **Scanners:** no dependency manifest changed, so `cargo deny`, `govulncheck` and `npm audit` report the same as main.

## Recovery rows (gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × fixed Parallel HITL pause spanning the approval-rule upgrade (wake or restore re-raises the open cards) | R (the same card bytes; Main replays) | `src/agents/events.rs` `fanout_card_parts` | the Worker card test, the Main real-PG replay test, browser (below) |

## Browser evidence (fanpar)

- **Stack.** fanpar, project 118, Claude in Chrome, local mock-OIDC test identity. Calls are counted from the mock MCP
  server's `jsonrpc tools/call` lines.
- **Before the upgrade.** Main and Worker were `pm-68fb160ef`, based on `0990ab10f`, before #1540.
  - Chat 45 ("FP parallel child direct", two Parallel members that run the saved "FP direct reverse"): the run parked
    with two `tool_guard` cards.
  - Cards `8fed4ba7…` (payload `88f78dce…`) and `c752d807…` (payload `54bf953a…`) were PENDING at revision 1.
  - Calls: 10.
- **The upgrade.** Main and Worker moved to `pm-8c7e1bb75`, built from this branch's commit in the build worktree.
  - The shared migrations ran up to #1540's 0211.
  - verify-profile: 49 passed, 1 failed. The failure is the known stale worktree comparison (ledger 211, worktree 204).
  - The fix adds no unique string literal, so image provenance is the build commit rather than a binary marker.
- **After the upgrade.**
  - Reload: both cards were still shown.
  - Authorize member (1): its call ran once (calls 11).
  - The wake republished the still-open card (2) as one `agent_interrupt_pending` frame. Main accepted it as a replay:
    the row stayed PENDING revision 1 with the same payload `54bf953a…`, and there was no raise conflict.
  - The run parked again (`stream_disposition="paused"`).
  - Block member (2): the run completed with the root `echo` (calls 12), and member (2)'s tool never ran.
  - Both cards are CONSUMED at revision 3, and the result survives a reload.

## Follow-ups

- None from this change. S1–S10 of the #1540 overlap review stay with that PR's owner.
