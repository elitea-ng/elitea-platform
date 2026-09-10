/**
 * Project Context (Settings > Project Context): a project-wide content blob
 * with an on/off toggle. The S1 ledger's onetest cases (ELITEA-0939/0943/
 * 0944/0945/0946/0948/0951/0952/0954) all ask the same underlying question —
 * does the CONTENT reach a turn's system prompt — but every one of them has a
 * precondition none of them state: that saving the content actually works.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * IT DOES NOT. MEASURED DIRECTLY AGAINST THE LIVE STACK, NOT ONLY READ FROM SOURCE.
 * ─────────────────────────────────────────────────────────────────────────────
 * `PUT /api/v2/elitea_core/project_context/prompt_lib/{projectId}/project-context`
 * answers 200 and ECHOES the body sent — `{"content": "…", "enabled": true}` —
 * which looks exactly like a successful save. A `GET` on the same route
 * immediately afterward returns `{"content": "", "enabled": false}`: the write
 * never reached the row it claims to have written.
 *
 * `internal/api/v2/eliteacore/handler.go`'s `UpdateProjectContext` explains
 * why. Its `INSERT` never names a `project_id` column:
 *
 *   INSERT INTO %s.configuration
 *     (elitea_title, label, type, data, section, status_ok, created_at)
 *   VALUES ('project_context_' || $1, 'Project Context', 'project_context',
 *           $2, 'project_context', true, NOW())
 *   ON CONFLICT (elitea_title) WHERE type = 'project_context' DO UPDATE …
 *
 * For a project with no PRE-EXISTING `project_context` row (any project that
 * has never saved one before — which is every project a fresh onetest run
 * would use), there is nothing for `ON CONFLICT` to match, so Postgres
 * attempts the bare `INSERT` — and the tenant schema's `configuration` table
 * requires `project_id NOT NULL`. Measured directly in this stack's own
 * Postgres log at the moment of the write:
 *
 *   ERROR: null value in column "project_id" of relation "configuration"
 *          violates not-null constraint
 *
 * The handler's own fallback (`UPDATE … WHERE type = 'project_context'`,
 * guarded by `_, _ = h.pool.Exec(...)  // fallback update; ignore error,
 * best-effort`) matches zero rows for the same reason and is not checked
 * either way — so the handler falls all the way through to `writeJSON(w,
 * http.StatusOK, map[string]any{"content": body.Content, "enabled":
 * body.Enabled})` regardless of whether either statement wrote anything. The
 * route cannot currently tell a caller "your save did not persist".
 *
 * This is a MORE FUNDAMENTAL gap than "the content is never woven into a
 * prompt" (`internal/application/agentexecution/memories.go`'s own doc
 * comment, and the admission-time `NOT EXISTS` gate in
 * `internal/db/sqlcgen/agent_chat.sql.go` that the earlier draft of this file
 * targeted): that gate can never even be REACHED, because the row it checks
 * for never gets written in the first place. Every onetest case in this
 * cluster — isolation, the per-agent Ignore toggle, multi-model, sub-agent
 * exclusion, direct-chat injection — collapses to this one root cause: the
 * save silently no-ops.
 *
 * Written as the onetest cases all implicitly assume (a save actually
 * persists) and marked `test.fail`, per the porting rulebook's "no
 * `test.skip` for a product gap" rule. It fails at the round trip itself —
 * PUT then GET — which is fast and needs no model turn at all, rather than at
 * a chat turn that a persistence failure makes meaningless to attempt (if the
 * content never saved, a turn answering normally proves nothing about
 * injection either way).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE CLEANUP IS A `finally`, UNLIKE THIS DIRECTORY'S OTHER SPECS
 * ─────────────────────────────────────────────────────────────────────────────
 * Every other journey here cleans up an ENTITY (an agent, a toolkit, a
 * pipeline) it alone created, so a cleanup skipped by an earlier failure only
 * strands that one row. Project Context is a single PROJECT-WIDE row. Given
 * the gap this file measures, the restore is unlikely to matter in practice
 * (the enabling write never took hold to begin with) — but on a project that
 * DOES already carry a `project_context` row (the `ON CONFLICT` branch, which
 * this bug does not affect), leaving it enabled would refuse every turn every
 * OTHER `chat-stream` spec sends afterwards (the admission gate this file's
 * header describes is real, even though this project can't currently reach
 * it). So the prior state is restored in a `finally`, unconditionally.
 *
 * WHY IT LIVES HERE: proving the round trip (as opposed to reading the Go
 * source) needs a real `elitea-main` instance and a real tenant schema — the
 * `journeys/**` stack could run this same assertion, but the finding was made
 * investigating this package's chat-turn cases, and the fix for whichever
 * onetest case ends up owning it will need the SAME stack this file already
 * runs on.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { AUTOTEST_PREFIX, readCallerPersonalProjectId } from '../fixtures/api';

interface ProjectContextBody {
  readonly content?: string;
  readonly enabled?: boolean;
}

function projectContextUrl(projectId: string): string {
  return `${BASE_URL}/api/v2/elitea_core/project_context/prompt_lib/${projectId}/project-context`;
}

test('Project Context, once saved with content enabled, reads back what was saved', async ({ page }) => {
  test.setTimeout(60_000);
  test.fail(
    true,
    'ELITEA-0954 (and the whole project-context cluster: 0939/0943/0944/0945/0946/0948/0951/0952): ' +
      'product gap — UpdateProjectContext\'s INSERT omits `project_id`, which the tenant `configuration` ' +
      'table requires NOT NULL; for a project with no pre-existing row the write fails silently ' +
      '(confirmed in this stack\'s own Postgres log: "null value in column \\"project_id\\" … violates ' +
      'not-null constraint") and the route still answers 200 with the body echoed back. A GET ' +
      'immediately after a PUT shows the save never took effect. See S/port/defects.md.',
  );

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona must work inside its own project').not.toBe('');

  // The state to RESTORE, read before this test changes anything.
  const before = await page.request.get(projectContextUrl(projectId));
  expect(before.ok(), 'the project context must be readable before this test changes it').toBe(true);
  const priorState = (await before.json()) as ProjectContextBody;

  try {
    const phrase = `${AUTOTEST_PREFIX}QA-TEST-PHRASE ${Date.now() % 1_000_000}: a violet tern circles the bay`;
    const saved = await page.request.put(projectContextUrl(projectId), {
      data: { content: phrase, enabled: true },
    });
    expect(saved.status(), `Project Context must be saveable: ${(await saved.text()).slice(0, 300)}`).toBeLessThan(
      300,
    );
    const savedBody = (await saved.json()) as ProjectContextBody;
    // The route's OWN claim: it echoes the body it was sent regardless of
    // whether it wrote anything, so this much always passes — the gap is
    // below, in the READ.
    expect(savedBody.enabled, 'the save response itself must claim the toggle is ON').toBe(true);
    expect(savedBody.content, 'the save response itself must echo the content sent').toBe(phrase);

    // THE assertion this file exists for: what a caller can actually observe
    // persisted, not what the write route claimed. This is where the gap is —
    // a working save would read back identically to what was just written.
    const readBack = await page.request.get(projectContextUrl(projectId));
    expect(readBack.ok(), 'the project context must be readable after saving it').toBe(true);
    const readBackBody = (await readBack.json()) as ProjectContextBody;
    expect(
      readBackBody,
      'Project Context must read back what was just saved — the enable/content pair must survive ' +
        'past the response that claimed to have written it',
    ).toEqual({ content: phrase, enabled: true });
  } finally {
    // Restored REGARDLESS of the assertion above, and before this test's
    // (expected) failure is allowed to end the test — see the header.
    const restored = await page.request.put(projectContextUrl(projectId), {
      data: { content: priorState.content ?? '', enabled: priorState.enabled ?? false },
    });
    expect(restored.ok(), 'Project Context must be restored, or a later chat-stream spec could be refused').toBe(
      true,
    );
  }
});
