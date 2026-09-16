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
 * ## ELITEA-2798/2799/2800: the round trip, now that it round-trips (#888)
 *
 * These three cases were one fail-mark, not three, because one defect made
 * them all unprovable: `UpdateProjectContext` wrote with an INSERT that
 * omitted `project_id` (NOT NULL on the tenant `configuration` table) under
 * an `ON CONFLICT (elitea_title) WHERE type = 'project_context'` clause
 * naming a partial unique index `001_initial.sql` never creates, discarded
 * both errors, and answered 200 with the SUBMITTED body echoed back. A PUT
 * looked perfect and every following GET answered the untouched default.
 *
 * The handler now writes UPDATE-first with `project_id` named, and answers a
 * typed 500 (`project_context_write_failed`) when it cannot write — so the
 * echo is no longer the only thing that ever looked right, and these three
 * cases are separable again:
 *
 *   ELITEA-2798 — a GET returns the value already stored;
 *   ELITEA-2799 — a full payload PUT round-trips;
 *   ELITEA-2800 — a partial update does not lose the other field.
 *
 * ELITEA-2800 needs a word: this route takes `{content, enabled}` as a whole
 * and has no partial-update form (no PATCH, and an omitted key decodes to
 * the zero value, not to "leave it alone"). The case's real question —
 * "changing one field does not silently destroy the other" — is asked here
 * in the shape this API has: a second PUT that keeps `content` and flips
 * `enabled` must leave `content` exactly as it was.
 *
 * ## Not sharing the row
 *
 * Project Context is ONE ROW PER PROJECT, and this file used to write project
 * 1's — as did `settings.project-context.spec.ts` under `fullyParallel: true`,
 * and as does every repeat of THIS test under `--repeat-each`. While nothing
 * persisted that was harmless; once the write became real it was a clobber,
 * and the cross-worker mutex both files then took only serialised the writes
 * (see `settings.project-context.spec.ts`'s header for what that did not fix).
 *
 * So this test provisions a project of its own (`fixtures/scratchProject.ts`)
 * and deletes it afterwards. Nothing here reads or writes project 1, and the
 * INSERT branch the case below is about is genuinely an insert: the project
 * has never held a row.
 */
import { expect, test } from '@playwright/test';

import { AUTOTEST_PREFIX } from '../../fixtures/api';
import { readProjectContext, writeProjectContext } from '../../fixtures/projectContext';
import { createScratchProject, deleteScratchProject, type ScratchProject } from '../../fixtures/scratchProject';

/** This test's own project — provisioned per test, deleted whatever the verdict. */
let scratch: ScratchProject | undefined;

test.beforeEach(async () => {
  scratch = await createScratchProject(`p13_${test.info().project.name}`);
});

test.afterEach(async () => {
  const project = scratch;
  scratch = undefined;
  await deleteScratchProject(project);
});

/** The test's own project, or a loud failure rather than a silent fallback to a shared one. */
function projectId(): string {
  if (scratch === undefined) throw new Error('the scratch project was not provisioned');
  return scratch.id;
}

/* onetest: ELITEA-2798, ELITEA-2799, ELITEA-2800 — the project-context round trip: a full payload
 * survives the PUT and comes back on the next GET (2799), a GET returns a value that was already
 * stored rather than a default (2798), and a second PUT that changes only `enabled` leaves `content`
 * intact (2800, asked in the shape this whole-object API has — see the file header). */
test('ELITEA-2798/2799/2800: a project-context PUT persists, reads back, and does not lose the other field', async ({
  request,
}) => {
  const target = projectId();
  // 2799 — the full payload, written into a project with NO prior row (it was
  // provisioned moments ago), which is the INSERT branch the missing
  // `project_id` column used to kill.
  const fullContent = `${AUTOTEST_PREFIX}p13-project-context-full-${Date.now()}`;
  await writeProjectContext(request, target, { content: fullContent, enabled: true });

  // 2798 — a GET returns what is stored. Read TWICE, because a handler that
  // answered from a per-request cache of its own write would satisfy one
  // read; the second read is a fresh request with nothing in front of it.
  expect(await readProjectContext(request, target)).toEqual({
    content: fullContent,
    enabled: true,
  });
  expect(await readProjectContext(request, target)).toEqual({
    content: fullContent,
    enabled: true,
  });

  // 2800 — change one field, keep the other. `content` must come back
  // byte-identical, not emptied by the write that was about `enabled`.
  await writeProjectContext(request, target, { content: fullContent, enabled: false });
  expect(await readProjectContext(request, target)).toEqual({
    content: fullContent,
    enabled: false,
  });

  // And the reverse edit, on the UPDATE branch: a new content value replaces
  // the old one rather than being appended as a second row the GET's
  // `LIMIT 1` would then choose between.
  const replaced = `${fullContent}-replaced`;
  await writeProjectContext(request, target, { content: replaced, enabled: true });
  expect(await readProjectContext(request, target)).toEqual({
    content: replaced,
    enabled: true,
  });
});

/*
 * THE REFUSAL HALF IS NOT REACHABLE FROM HERE, and is not silently dropped.
 *
 * The write now answers a typed 500 (`project_context_write_failed`) instead
 * of the 200-with-an-echo that hid #888. Asking for it over HTTP means naming
 * a project whose tenant schema does not exist — and `projectScoped` refuses a
 * project the caller is not a member of with 403 BEFORE the handler runs, so
 * no request from a browser session can reach that branch. Measured: 403, not
 * 500.
 *
 * It is covered where it can be: `services/elitea-main/internal/api/v2/
 * eliteacore/project_context_postgres_integration_test.go`'s
 * TestProjectContextRefusesAWriteItCannotPersist, which mounts the handler
 * without the permission middleware and asserts both the status and that the
 * body leaks no table name or SQLSTATE.
 */
