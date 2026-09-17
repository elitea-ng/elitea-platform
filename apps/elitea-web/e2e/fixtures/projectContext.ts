/**
 * Project Context: the raw read/write pair.
 *
 * ## Why there is no lock here any more
 *
 * Project Context is ONE ROW PER PROJECT (`p_<id>.configuration` where
 * `type = 'project_context'`). While the write silently no-opped (#888) every
 * journey could share project 1: the row never changed, so "the project starts
 * empty and stays empty" was true for every test, in any order, at any
 * concurrency.
 *
 * Making the write real turned that shared row into shared MUTABLE state under
 * `fullyParallel: true`, and the first answer was a cross-worker mutex over it
 * (`mutex.ts`, under the name `project-context`). It did not hold: the file it
 * was written for went on failing a DIFFERENT test on each CI run. A lock
 * serialises the WRITES and nothing else — it cannot stop the page's React
 * Query cache and one-shot mount fetch from having read the row a moment
 * before the lock changed hands, and it leaves every test depending on the
 * previous holder having reset the row.
 *
 * Both callers now provision a project of their own instead
 * (`scratchProject.ts`), so no row is shared and there is nothing to
 * serialise. `mutex.ts` itself stays — `settings.pat-expiry-notifications`
 * holds a genuinely deployment-wide window through it.
 */
import { request as apiRequest, type APIRequestContext } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../playwright.config';
import { API_BASE } from './api';

/** What `{content, enabled}` the route answers for a project that has never saved one. */
export interface ProjectContextState {
  readonly content: string;
  readonly enabled: boolean;
}

/**
 * The state every project on a freshly seeded stack is in, and the state
 * `resetProjectContext` puts one back into.
 *
 * `enabled: false` with empty content is what `ProjectContext.tsx`'s
 * `deriveShowFlags` treats as "nothing saved": the empty state renders and
 * the editor does not mount until the toggle is flipped or Create is clicked.
 */
export const EMPTY_PROJECT_CONTEXT: ProjectContextState = { content: '', enabled: false };

export function projectContextUrl(projectId: string): string {
  return `${API_BASE}/elitea_core/project_context/prompt_lib/${projectId}/project-context`;
}

/**
 * Reads the row the SERVER holds, straight off the route.
 *
 * The body is the plain `{content, enabled}` object — NOT `{data: {...}}`.
 * The `{data}` wrapper belongs to the browser client (`eliteaFetch` builds
 * it; `projectContextApi.ts` unwraps it), so a `page.request` reader that
 * reaches for `body.data.content` gets `undefined` and, with a `?? ''`
 * fallback, reports "" for every project forever — indistinguishable from
 * the very no-op-save defect (#888) such a reader is usually written to
 * measure. Verified against the running stack with curl before this helper
 * was written.
 */
export async function readProjectContext(
  api: APIRequestContext,
  projectId: string,
): Promise<ProjectContextState> {
  const response = await api.get(projectContextUrl(projectId));
  if (!response.ok()) {
    throw new Error(
      `GET project context for project ${projectId}: ${String(response.status())} ${await response.text()}`,
    );
  }
  const body = (await response.json()) as Partial<ProjectContextState>;
  return { content: body.content ?? '', enabled: body.enabled ?? false };
}

/** PUTs `{content, enabled}` and throws on any non-200 — a refused save must not read as an empty one. */
export async function writeProjectContext(
  api: APIRequestContext,
  projectId: string,
  state: ProjectContextState,
): Promise<void> {
  const response = await api.put(projectContextUrl(projectId), { data: state });
  if (!response.ok()) {
    throw new Error(
      `PUT project context for project ${projectId}: ${String(response.status())} ${await response.text()}`,
    );
  }
}

/**
 * Puts one project's row back to the empty default, as ADMIN.
 *
 * Admin rather than the calling test's own persona because the viewer
 * persona (`STORAGE_STATE.viewer`, which holds `models.project_context.view`
 * and specifically NOT `.edit`) must still be able to arrange its own
 * precondition — the whole point of that persona is that it cannot write.
 */
export async function resetProjectContext(projectId: string): Promise<void> {
  await seedProjectContextAsAdmin(projectId, EMPTY_PROJECT_CONTEXT);
}

/** Saves one project's row as admin, for a test whose own persona may not write. */
export async function seedProjectContextAsAdmin(
  projectId: string,
  state: ProjectContextState,
): Promise<void> {
  const admin = await apiRequest.newContext({
    baseURL: BASE_URL,
    storageState: STORAGE_STATE.admin,
  });
  try {
    await writeProjectContext(admin, projectId, state);
  } finally {
    await admin.dispose();
  }
}
