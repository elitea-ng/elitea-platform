/**
 * Project Context (Settings > Project Context): a project-wide content blob
 * with an on/off toggle. The S1 ledger's onetest cases (ELITEA-0939/0943/
 * 0944/0945/0946/0948/0951/0952/0954) all ask the same underlying question —
 * does the CONTENT reach a turn's system prompt — but every one of them has a
 * precondition none of them state: that saving the content actually works.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * IT DID NOT, UNTIL #888. THE ROUND TRIP IS THE PRECONDITION, AND IT IS TESTED HERE.
 * ─────────────────────────────────────────────────────────────────────────────
 * `PUT /api/v2/elitea_core/project_context/prompt_lib/{projectId}/project-context`
 * used to answer 200 and ECHO the body sent — `{"content": "…", "enabled":
 * true}` — which looked exactly like a successful save, while a `GET` on the
 * same route immediately afterward returned `{"content": "", "enabled":
 * false}`. `UpdateProjectContext`'s INSERT never named a `project_id` column,
 * which the tenant `configuration` table requires NOT NULL, so on any project
 * with no pre-existing row the write died in the database:
 *
 *   ERROR: null value in column "project_id" of relation "configuration"
 *          violates not-null constraint
 *
 * — and the handler's own fallback (`UPDATE … WHERE type = 'project_context'`,
 * under `_, _ = h.pool.Exec(...)  // ignore error, best-effort`) matched zero
 * rows for the same reason and was not checked either way. Both errors were
 * discarded and the 200 was written regardless.
 *
 * The handler now writes UPDATE-first with `project_id` named, and returns a
 * typed 500 (`project_context_write_failed`) rather than swallowing a failed
 * write. The round trip below is no longer a fail-marked product gap: it is
 * the PRECONDITION every injection case in this cluster (ELITEA-0939/0943/
 * 0944/0945/0946/0948/0951/0952/0954) silently assumes, asserted directly,
 * without needing a model turn.
 *
 * What is STILL not proven by this file is the injection itself — that the
 * saved content reaches a turn's system prompt (`memories.go`), and that the
 * admission-time `NOT EXISTS` gate in `agent_chat.sql.go` behaves. Those need
 * a real model turn. With the save fixed, the gate is at least REACHABLE now,
 * which it was not before.
 *
 * NOTE ON WHERE THIS RAN. This package's own stack (`chat-stream`) is not
 * available in the wave that fixed #888, so the assertion below was flipped
 * off `test.fail` on the strength of the same round trip proven two other
 * ways: `services/elitea-main/internal/api/v2/eliteacore/
 * project_context_postgres_integration_test.go` (PUT-then-GET through the
 * handler on the real migration corpus, both the no-prior-row and the
 * existing-row branch) and `journeys/settings/settings.p13-project-context.
 * spec.ts` (the same round trip over HTTP against the journeys stack).
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

/* onetest: ELITEA-0954 — and the precondition half of ELITEA-0939/0943/0944/0945/0946/0948/0951/0952:
 * Project Context saved with content and the toggle on is READABLE BACK, which every injection case in
 * this cluster assumes without stating. The injection itself (does the content reach the turn's system
 * prompt) needs a model turn and is not asserted here — see the file header. */
test('Project Context, once saved with content enabled, reads back what was saved', async ({ page }) => {
  test.setTimeout(60_000);

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
    // The route's OWN claim. It echoed the body it was sent regardless of
    // whether it wrote anything, so this much always passed even while the
    // write was a no-op — the assertion that discriminates is the READ.
    expect(savedBody.enabled, 'the save response itself must claim the toggle is ON').toBe(true);
    expect(savedBody.content, 'the save response itself must echo the content sent').toBe(phrase);

    // THE assertion this file exists for: what a caller can actually observe
    // persisted, not what the write route claimed. A working save reads back
    // identically to what was just written.
    const readBack = await page.request.get(projectContextUrl(projectId));
    expect(readBack.ok(), 'the project context must be readable after saving it').toBe(true);
    const readBackBody = (await readBack.json()) as ProjectContextBody;
    expect(
      readBackBody,
      'Project Context must read back what was just saved — the enable/content pair must survive ' +
        'past the response that claimed to have written it',
    ).toEqual({ content: phrase, enabled: true });
  } finally {
    // Restored REGARDLESS of the assertion above — see the header. It
    // matters more now than it did: the save it undoes actually persists.
    const restored = await page.request.put(projectContextUrl(projectId), {
      data: { content: priorState.content ?? '', enabled: priorState.enabled ?? false },
    });
    expect(restored.ok(), 'Project Context must be restored, or a later chat-stream spec could be refused').toBe(
      true,
    );
  }
});
