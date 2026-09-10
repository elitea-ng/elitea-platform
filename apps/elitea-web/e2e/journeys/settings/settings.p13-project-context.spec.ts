/**
 * P13 rejudge — `project-context` folder (ELITEA-2798, 2799, 2800; ELITEA-
 * 2802 in THIS folder is the `delete_project_context` case, distinct from
 * the SAME id in the `indexing` folder — see `toolkits.p13-run-history-
 * trace.spec.ts` for that one). Settings › Project Params › Project Context
 * (`pages/settings/ProjectContext.tsx`) is a plain REST-backed GET/PUT pair
 * — `GET/PUT /elitea_core/project_context/prompt_lib/{projectId}/
 * project-context` — no model/chat turn involved, so this is request-only,
 * the same technique `admin.agent-publishing-guardrails.spec.ts` uses for
 * its own section.
 *
 * ELITEA-2802 (delete_project_context) is recorded NA, not tested here: no
 * `deleteProjectContext` function exists in the generated client at all, and
 * no DELETE route is described for `/elitea_core/project_context/...`.
 *
 * ELITEA-2805/2806/2808 (`generate_project_context_draft` — a real model
 * call, 2808 explicitly a real-provider compatibility matrix) are LIVE-ONLY.
 *
 * ## ELITEA-2798/2799/2800 are ALL one confirmed, present-tense bug: a PUT
 * to Project Context never persists at all, on this stack or any other
 *
 * `UpdateProjectContext` (`internal/api/v2/eliteacore/handler.go`) writes
 * with:
 *   INSERT INTO %s.configuration (elitea_title, ...) VALUES ('project_context_' || $1, ...)
 *   ON CONFLICT (elitea_title) WHERE type = 'project_context' DO UPDATE SET data = $2
 * `ON CONFLICT (col) WHERE …` names a PARTIAL unique index — but
 * `configuration.elitea_title` (`internal/infra/db/migrations/001_initial.sql:823`)
 * is a plain, non-partial `UNIQUE` constraint. Postgres rejects an ON
 * CONFLICT target that names a predicate no matching index actually has —
 * "there is no unique or exclusion constraint matching the ON CONFLICT
 * specification" — on EVERY call, insert or not; the error is swallowed
 * (`_, err := h.pool.Exec(...)`), and the fallback `UPDATE … WHERE type =
 * 'project_context'` finds no row (the INSERT never landed) and silently
 * affects zero rows. The handler then answers 200 with the SUBMITTED body
 * echoed back (`writeJSON(w, http.StatusOK, map[string]any{"content":
 * body.Content, "enabled": body.Enabled})`) regardless of what, if
 * anything, actually reached the table — measured directly against the
 * running stack: a PUT answers 200 with the exact body sent, and every
 * following GET answers the untouched default `{"content": "", "enabled":
 * false}`, forever. This makes ELITEA-2798 (get returns a pre-existing
 * value), ELITEA-2799 (put + get round-trips a full payload) and ELITEA-2800
 * (partial update preserves content) all unprovable on the SAME single root
 * cause — one FAIL-MARK, not three, per the write's own echo being the only
 * thing that ever looked right.
 */
import { expect, test } from '@playwright/test';

import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

const CONTEXT_URL = `${API_BASE}/elitea_core/project_context/prompt_lib/${DEFAULT_PROJECT_ID}/project-context`;

interface ProjectContextBody {
  readonly content?: string;
  readonly enabled?: boolean;
}

/* onetest: ELITEA-2798, ELITEA-2799, ELITEA-2800 — PRODUCT GAP, see file header: a project-context PUT
 * never actually persists (an invalid ON CONFLICT target vs. a plain UNIQUE constraint), so get after
 * put never shows what was written, a full-payload round-trip does not round-trip, and the
 * partial-update-preserves-content question is moot — nothing survives a PUT at all. */
test('ELITEA-2798/2799/2800: a project-context PUT never actually persists — get after put shows the untouched default', async ({
  request,
}) => {
  test.fail(
    true,
    'ELITEA-2798/2799/2800: product gap — UpdateProjectContext\'s ON CONFLICT (elitea_title) WHERE ' +
      "type = 'project_context' names a partial unique index that does not exist (elitea_title carries " +
      'only a plain UNIQUE constraint, migrations/001_initial.sql:823), so the write silently no-ops ' +
      'and the 200 response merely echoes the submitted body — get_project_context never returns what ' +
      'was just put_project_context\'d, on a full payload or a partial one',
  );

  const fullContent = `${AUTOTEST_PREFIX}p13-project-context-full-${Date.now()}`;
  const put = await request.put(CONTEXT_URL, { data: { content: fullContent, enabled: true } });
  expect(put.status(), await put.text()).toBe(200);

  const got = await request.get(CONTEXT_URL);
  expect(got.status(), await got.text()).toBe(200);
  const body = (await got.json()) as ProjectContextBody;
  // What the case says SHOULD hold (2798's read-back, 2799's round-trip).
  expect(body.content).toBe(fullContent);
  expect(body.enabled).toBe(true);
});
