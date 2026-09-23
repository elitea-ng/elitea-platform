/**
 * A REAL, provisioned project that belongs to ONE test, and is gone when it ends.
 *
 * ## Why this exists
 *
 * Several settings live at "one row per project" — Project Context is the
 * clearest: `p_<id>.configuration` holds exactly one `project_context` row and
 * the page reads and writes THAT row. While such a write no-opped (#888) every
 * journey could share project 1 and still pass; once the write became real,
 * four workers and two engines running the same file wrote one row and read
 * each other's values. That is a CLOBBER, not a flake a retry fixes, and a
 * cross-worker mutex only trades it for a queue — it still leaves every test
 * depending on the previous holder having reset the row, and it cannot stop the
 * page's own React Query cache from having read the row a moment before the
 * lock changed hands. (Measured: `settings.project-context.spec.ts` failed a
 * DIFFERENT test on each CI run under exactly that arrangement.)
 *
 * The fix is to stop sharing. Provisioning is not expensive here — measured
 * against the e2e stack, `POST /projects/project/administration` answers 201 in
 * ~250 ms and `DELETE …/{id}` in ~100 ms, because the nine steps are SQL
 * (row, `p_<id>` schema and its migration chain, roles, system user and token,
 * vault, buckets, pgvector) against a warm database — so a project per TEST is
 * affordable, and a test that owns its project shares no row with anything.
 *
 * ## What a scratch project comes with
 *
 * - the ADMIN and MEMBER personas as project admins, through the create route's
 *   own `project_admin_email` field (`AdminRoles: ["admin"]` is hardcoded
 *   server-side — a caller does not choose the role it grants);
 * - the VIEWER persona as a project `viewer`, added through the admin members
 *   route, so the read-only journeys have their persona here too.
 *
 * The viewer is genuinely restricted WITHOUT the revoke `scripts/e2e-stack.sh`
 * has to perform on project 1. That revoke exists because project 1 carries
 * per-project `auth_core__project_role_permission` overrides that predate any
 * real viewer. A freshly provisioned project has NO override rows at all (see
 * `createProjectPermissions`' own note: writing them would suppress
 * legacyrbac's central fallback), so its roles resolve the CENTRAL grants —
 * where `models.project_context.edit` reaches `admin`/`editor` only
 * (shared/0068) and `models.project_context.view` reaches every role
 * (shared/0062). Measured against the stack: viewer GET 200, viewer PUT 403.
 */
import { request as apiRequest, type APIRequestContext } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX } from './api';

/** The seeded persona accounts (`scripts/e2e-stack.sh seed`). */
const ADMIN_EMAIL = 'e2e-admin@autotest.local';
const MEMBER_EMAIL = 'e2e-member@autotest.local';
const VIEWER_EMAIL = 'e2e-viewer@autotest.local';

export interface ScratchProject {
  /** The provisioned project's id, as the string every project-scoped route takes. */
  readonly id: string;
  /** Its name — what the sidebar switcher lists it under, for `ensureProjectSelected`. */
  readonly name: string;
}

/**
 * The name a scratch project is created under.
 *
 * `label` names the file that asked for it, and the rest makes the name unique
 * across engines, workers and repeats: two Playwright projects run the same
 * file at the same time, and `--repeat-each` runs it several times within one.
 *
 * Deliberately NOT starting with `project_user_`: the admin listing splits
 * personal from team projects on exactly that prefix.
 */
function scratchProjectName(label: string): string {
  const unique = `${String(Date.now())}${String(Math.floor(Math.random() * 1_000))}`;
  return `${AUTOTEST_PREFIX}scratch_${label}_${unique}`.replace(/[^a-z0-9_]/gi, '_');
}

/** An admin request context — the personas' own sessions cannot provision. */
async function adminApi(): Promise<APIRequestContext> {
  return apiRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.admin });
}

/**
 * How many times a provisioning call is attempted.
 *
 * Provisioning and deprovisioning are multi-statement pipelines over shared
 * tables, and four workers running them at once can meet each other:
 * `project provisioning compensation failed … delete project rows: deadlock
 * detected (SQLSTATE 40P01)` was logged by elitea-main during this file's own
 * bring-up. A deadlock is the database electing a victim, so the loser's
 * retry succeeds; two attempts is enough for that and still fails loudly on
 * anything systematic.
 *
 * It is NOT what keeps the ids apart. `POST /projects/project/administration`
 * lets the sequence allocate the id, and the seeded rows name theirs
 * explicitly, so the sequence used to walk into an id the seed had already
 * taken (measured: a 23505 on `project_pkey` at id 99). That is fixed where it
 * belongs — `scripts/e2e-stack.sh` now advances the sequence past every
 * hand-seeded id — not by retrying until a different number comes out.
 */
const ATTEMPTS = 2;

const sleep = async (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/**
 * Provisions a project of this test's own and returns it.
 *
 * `label` should identify the caller (a spec or a test id) so a leaked project
 * — a worker killed mid-test is the only way to leak one — names its origin.
 */
export async function createScratchProject(label: string): Promise<ScratchProject> {
  const api = await adminApi();
  try {
    let refusal = '';
    for (let attempt = 1; attempt <= ATTEMPTS; attempt++) {
      // A fresh NAME per attempt: the first attempt may have rolled back only
      // partly, and a name is how a leaked project is traced back here.
      const name = scratchProjectName(label);
      const created = await api.post(`${API_BASE}/projects/project/administration`, {
        data: { name, project_admin_email: [ADMIN_EMAIL, MEMBER_EMAIL] },
        timeout: 120_000,
      });
      if (created.status() !== 201) {
        // The body carries one status per provisioning step and names the one
        // that stopped — a report of "not 201" alone would hide it.
        refusal = `POST -> ${String(created.status())} ${(await created.text()).slice(0, 600)}`;
        if (attempt < ATTEMPTS) await sleep(500);
        continue;
      }
      const body = (await created.json()) as { id?: number };
      if (typeof body.id !== 'number') {
        throw new Error(
          `createScratchProject(${name}): the create answered no id: ${JSON.stringify(body).slice(0, 400)}`,
        );
      }
      return await grantViewer(api, { id: String(body.id), name });
    }
    throw new Error(`createScratchProject(${label}): ${String(ATTEMPTS)} attempts, last: ${refusal}`);
  } finally {
    await api.dispose();
  }
}

/** Adds the restricted persona to a project that has just been provisioned. */
async function grantViewer(api: APIRequestContext, project: ScratchProject): Promise<ScratchProject> {
  // The restricted persona. Its own journeys must not be the ones that arrange
  // this, precisely because that persona cannot write.
  const invited = await api.post(`${API_BASE}/admin/users/administration/${project.id}`, {
    data: { emails: [VIEWER_EMAIL], roles: ['viewer'] },
    timeout: 60_000,
  });
  if (!invited.ok()) {
    // A project whose membership is wrong is worse than no project: the test
    // would fail on a permission assertion that is about the product.
    await deleteScratchProject(project);
    throw new Error(
      `createScratchProject(${project.name}): adding the viewer -> ${String(invited.status())} ${(await invited.text()).slice(0, 400)}`,
    );
  }
  return project;
}

/**
 * Deprovisions a scratch project, dropping its tenant schema with it.
 *
 * Never throws: it runs in `afterEach`/`finally`, where an exception would
 * replace the test's own failure with this one. A refusal is reported instead,
 * because a project that outlives its test leaves a `p_<id>` schema behind.
 */
export async function deleteScratchProject(project: ScratchProject | undefined): Promise<void> {
  if (project === undefined) return;
  const api = await adminApi();
  try {
    for (let attempt = 1; attempt <= ATTEMPTS; attempt++) {
      const deleted = await api.delete(`${API_BASE}/projects/project/administration/${project.id}`, {
        timeout: 120_000,
      });
      if (deleted.ok()) return;
      if (attempt < ATTEMPTS) {
        await sleep(500);
        continue;
      }
      // eslint-disable-next-line no-console -- a silent teardown is how a real tenant schema outlives its own test
      console.warn(
        `deleteScratchProject(${project.name}): ${String(deleted.status())} ${(await deleted.text()).slice(0, 300)}`,
      );
    }
  } catch (error) {
    // eslint-disable-next-line no-console -- same reason: teardown must not mask the test's own verdict
    console.warn(`deleteScratchProject(${project.name}) threw: ${String(error)}`);
  } finally {
    await api.dispose();
  }
}
