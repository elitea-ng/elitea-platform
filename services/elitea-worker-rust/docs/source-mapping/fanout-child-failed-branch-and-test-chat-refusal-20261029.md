# FU-249 follow-up: a failed branch in a child-pipeline wake, and the editor test chat's refusal text (2026-10-29)

Branch `fix/fanout-nested-wake-failed-branch-card` off `main` 5e9c6dfa8 (FU-249, #1524).

## Business behaviour

- **A branch failing during a wake keeps the other branches' cards.** A fixed Parallel inside a saved child pipeline
  parks with one card per branch. When the answered branch's resumed model call fails in the wake (On Error
  `continue`), that branch ends failed, the still-open branches are re-raised and the run parks again. The live page
  keeps every open card; nothing waits for a reload.
- **The pipeline editor's test chat names a refused composition.** Starting the editor test chat of a pipeline that
  Main refuses (for example "FP nested", whose Parallel branch runs a saved pipeline holding a Parallel) shows Main's
  named refusal instead of "Could not start a test conversation".
- The current platform has neither surface (no Parallel node); nothing is ported from it.

## What was found

- **Failed branch (reported as a live card loss in #1524).** Not reproducible. Three independent checks keep the card:
  1. the Worker's own frames (real-PG test below);
  2. the original failing browser run's frames, exported from Main's `elitea_runtime.execution_replay_events`
     and replayed through the Web reducer;
  3. a live repro on fanpar with the LLM gateway paused during the wake.

  In all three, the open card leaves only on the user's own later answer (`agent_hitl_resolved`). The single earlier
  sighting happened in the session whose Chrome debugging channel wedged (`2a2/evidence/browser-cases.md`). This
  change therefore adds regression tests and no reducer change.
- **Editor test chat refusal.** Main answered the refusal as `400 {"error": …}`. Conversation create went through
  `apierr.Write`, which never writes the code. The Web shows Main's text only for `code:
  APPLICATION_COMPOSITION_INVALID` (`apps/elitea-web/src/pages/pipelines/lib/usePipelineTestConversation.ts:59`), so
  it fell back to the generic retry hint. The attach route already writes `{error, code}`
  (`internal/api/v2/eliteacore/application_relation.go:388`).

## Changed paths

- `services/elitea-main/internal/api/v2/conversations/handler.go`
  - `writeRepoError` (`:2218`);
  - used by Create (`:829`), AddParticipants (`:1525`) and UpdateEntitySettings (`:1675`).
- `services/elitea-main/internal/api/v2/conversations/composition_refusal_test.go` (new).
- `services/elitea-worker-rust/src/agents/pipeline_fanout_effect_pg_tests.rs`
  - `postgres_a_branch_failing_in_a_child_pipeline_wake_keeps_the_other_cards` (`:1659`).
- `apps/elitea-web/src/features/chat-messages/lib/chatStreamNestedWake.test.ts`
  - `a wake whose answered branch fails` (`:62`).
- `apps/elitea-web/src/features/chat-messages/__tests__/nested-wake-failed-frames.json` (new)
  - the 66 frames of the original run: park `270f111d…` plus wake `9f00b109…`, as Main replays them;
  - only `type`, `content`, `response_metadata` and `execution_generation` are kept;
  - scanned: no token, key, e-mail or personal data.

## Tests

- **Main:**
  - `TestCompositionRefusalCarriesItsCode` covers create, participant add and participant settings;
  - `TestOtherRefusalsKeepThePlainBody` checks that any other refusal still returns the plain body;
  - TDD: the code test failed first ("expected the coded refusal, got map[error:…]");
  - `go test -race ./...`: see the PR.
- **Worker:**
  - clippy `-D warnings` passes;
  - the `fanout_effect_pg_tests` module passes 25/0 on real PostgreSQL, including the new test (0 skipped).
- **Web:** `chatStreamNestedWake.test.ts` 2/0; full vitest: see the PR. oxlint and `tsc --noEmit` are clean.

## Performance

- **Main:** `writeRepoError` runs only on the error path. It does one `errors.As` and writes one JSON object, with no
  new I/O. The success path is unchanged.
- **Worker/Web:** test-only.

## Durability

- No storage change. The refusal is raised before any write in the conversation transaction (unchanged), so a refused
  create still stores nothing. The browser check after a reload shows no test conversation.
- The failed-branch wake keeps the 2a-2/FU-249 durability unchanged. Ledger after the live repro:
  - bell `CONSUMED` once;
  - lights `PENDING` until answered, then `CONSUMED`.

## Resilience

- A typed refusal stays typed end to end: `APPLICATION_COMPOSITION_INVALID` with Main's text. Any other failure keeps
  the generic retry hint. Proven by `TestOtherRefusalsKeepThePlainBody` and the existing Web test "keeps the retry
  hint for any other failure".
- A failed branch is data under On Error `continue`: the run is not failed, and the other branches park again. Proven
  by the new PG test.

## Security

- **No new input surface.** The refusal text is Main's own registered message, built from saved application names,
  as on the attach route. It goes to the same actor who could already read those names.
- **No authorization change.** The refusal path runs after the existing authorization and inside the same
  transaction.
- **The fixture holds no secret, token or personal data** (pattern scan recorded above).
- `rules/security.md` checklist: not applicable beyond the above:
  - no identity, egress, parser, SQL or credential change;
  - Web rendering unchanged (React text, no HTML).

## Recovery rows (replatform-delivery-gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × wake: answered child branch's resumed model call fails (On Error `continue`) | R for the open branches; the failed branch joins as a failed entry (data) | FU-249 park/restore (`runner.rs:425`) | `postgres_a_branch_failing_in_a_child_pipeline_wake_keeps_the_other_cards`; live fanpar run 4 |
| Web × live view during that wake | R (card kept live) | `chatStreamFanoutFrames.ts:179` (member status only), `useInterruptLedger.ts:127` (re-read at each park) | real-frame replay test; live run 4 |
| Main × conversation create refused by composition | F (typed, readable, nothing stored) | `conversations/handler.go:2218` | `TestCompositionRefusalCarriesItsCode`; browser |

## Browser evidence (fanpar)

- **Images:**
  - Worker `2a2-73627a9f7`, Web `2a2-cd8b5fd58`;
  - Main first `2a2-cd8b5fd58`, then verify image `2a2v-722a9afca` (= `cd8b5fd58` plus the Main fix). Its id is
    sha256:962ed110…6714. `stack.sh binary-check` finds `conversations.writeRepoError` once in it and not in
    `2a2-cd8b5fd58`.
- **verify-profile:** 49/1 after the Main swap. The one failure is the worktree-head migration check
  (`ledger=198 worktree=202`): the worktree now holds `main`, where #1470's migration is 0202, while fanpar's images
  and ledger are at 198.
- **Failed branch, chat 20 run 4** (response `d5a22bb5…`):
  - two cards; bell answered "copper" with the LLM gateway paused 11:18:23–11:19:08;
  - wake `de2b38a9…`: bell `agent_tool_error`, `bell:failed` at 11:18:49, lights re-parked;
  - the lights card never left the live page (DOM sampler, fetch hook: `GET …/interrupts` 200 at each park);
  - lights answered "amber" → "Pipeline completed."; reload keeps it.
- **Attach refusal:** attaching "FP inner parallel" to agent "Instruction revision inspector" is refused inline with
  the new text. After a reload nothing is attached.
- **Editor test chat refusal, "FP nested":**
  - before the fix: generic "Could not start a test conversation" (400 body without `code`);
  - with `2a2v-722a9afca`: the panel shows Main's text, "Pipeline "FP inner parallel" has Parallel node "inner", so
    it cannot run as a tool of an agent or inside a Parallel branch ("FP nested" → "FP inner parallel"). Run …";
  - after a reload no test conversation is stored.

## Follow-ups

- Main logs every minute "execution replay retention cycle failed … `execution_replay_state_last_event_complete`
  (23514)" on fanpar. Seen in passing; not this change.
- Known cosmetic, unchanged: after a reload, the quoted "1. <question>" line of a completed nested run can show a
  different branch's question than the live view did.
