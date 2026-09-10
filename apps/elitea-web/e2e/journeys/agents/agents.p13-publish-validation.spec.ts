/**
 * P13 rejudge — `agents-publishing` folder, ELITEA-0158 (the earlier
 * `agents.publish-validation.spec.ts` carries the SAME id tagged against a
 * different case — an "Editor Notes field" product gap — because the
 * onetest source reuses ids across folders; see that file's own doc comment
 * and `S/port/ledger-P13-rejudge.tsv`'s folder column. This file judges
 * 0158's `agents-publishing` body: "Publishing blocked when a project-
 * specific (private) model is selected on main agent or sub-agent."
 *
 * Read directly from `runPublishValidation`
 * (`internal/api/v2/eliteacore/handler.go`, "Check LLM model is from an
 * accessible project" block): the check runs ONCE, against the version
 * passed to `POST /publish_validate/...` directly — never against a
 * sub-agent's own `llm_settings`. So:
 *
 *  - Variant A (main agent's own model is project-specific) is REAL: a
 *    critical issue on field `llm_settings`. The message is NOT the one the
 *    onetest source guessed ("Model '[name]' used by parent agent is
 *    project-specific…") — the real sentence is "model is not shared and
 *    cannot be used in published agents" — this test asserts the real one.
 *  - Variant B (a SUB-agent's model is project-specific, main agent's own
 *    model is valid) is a product gap: `validateSubAgents` — the recursive
 *    closure that walks sub-agents — only ever checks `instructions` length,
 *    `description` length and sub-agent name uniqueness. It never reads the
 *    sub-agent's `llm_settings` at all, so a sub-agent's private model is
 *    silently allowed through validation. Variant C (two sub-agents, joined
 *    error) does not apply — there is no per-sub-agent error to join at all.
 *
 * Both variants go through `POST /publish_validate/prompt_lib/{project}/
 * {versionId}` directly, the same request-only technique
 * `agents.publish-validation.spec.ts`'s own header explains and its ELITEA-
 * 0159 test already uses — no wizard UI, no chat/model turn.
 */
import { test, expect, type APIRequestContext, type APIResponse } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  attachSubAgent,
  createAgentWithVersion,
  deleteAgent,
  readProjectModels,
  resolveCatalogueProjectId,
  resolvePrivateModel,
} from '../../fixtures/api';

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}p13pubval-${stem}-${String(Date.now()).slice(-7)}`;
}

interface ValidationFinding {
  readonly field?: string;
  readonly issue?: string;
  readonly context?: string;
}
interface ValidationResult {
  readonly status?: string;
  readonly critical_issues?: readonly ValidationFinding[];
  readonly warnings?: readonly ValidationFinding[];
}

/** POSTs the wizard's own validation route directly — see file header. */
async function validate(
  request: APIRequestContext,
  versionId: string,
  versionName: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<{ readonly status: number; readonly body: ValidationResult }> {
  const resp: APIResponse = await request.post(
    `${API_BASE}/elitea_core/publish_validate/prompt_lib/${projectId}/${versionId}`,
    { data: { version_name: versionName, category: 'Development' } },
  );
  const body = (await resp.json()) as ValidationResult;
  return { status: resp.status(), body };
}

const PASSABLE_INSTRUCTIONS =
  'You are a meeting preparation assistant. Turn the notes, transcripts and agendas the user ' +
  'gives you into a short briefing that names the people involved and the decisions still open.';

test.describe('publish validation: project-specific model (ELITEA-0158)', () => {
  /* onetest: ELITEA-0158 — Variant A: the MAIN agent's own model is project-specific (not the shared
   * Public catalogue) — publish validation raises a critical `llm_settings` finding. */
  test('a main agent on a project-specific model is refused with a real llm_settings finding', async ({
    request,
  }) => {
    const privateModel = await resolvePrivateModel(request);
    const agent = await createAgentWithVersion(request, uniqueName('mainprivate'), {
      instructions: PASSABLE_INSTRUCTIONS,
      model: { modelName: privateModel.modelName, modelProjectId: privateModel.projectId },
    });
    try {
      const { status, body } = await validate(request, agent.versionId, uniqueName('ver'));
      expect(status, JSON.stringify(body)).toBe(422);
      expect(body.status).toBe('FAIL');
      const llmFindings = (body.critical_issues ?? []).filter((finding) => finding.field === 'llm_settings');
      expect(llmFindings.length, `expected an llm_settings critical issue: ${JSON.stringify(body)}`).toBeGreaterThan(0);
      expect(llmFindings[0]?.issue).toBe('model is not shared and cannot be used in published agents');
    } finally {
      await deleteAgent(request, agent.id);
    }
  });

  /* onetest: ELITEA-0158 — Variant B: PRODUCT GAP — a SUB-agent's project-specific model raises no
   * finding at all. `validateSubAgents` (handler.go) only ever inspects a sub-agent's instructions/
   * description length and name uniqueness; it never reads the sub-agent's own `llm_settings`, so a
   * sub-agent on a private model is silently allowed through, contrary to what this case (and its own
   * Variant A, immediately above) would lead an operator to expect. */
  test('a sub-agent on a project-specific model is NOT caught by publish validation', async ({ request }) => {
    test.fail(
      true,
      'ELITEA-0158 Variant B: product gap — runPublishValidation never inspects a sub-agent\'s own ' +
        'llm_settings (validateSubAgents only checks instructions/description length and name ' +
        'uniqueness), so a sub-agent on a project-specific model raises no llm_settings finding at all',
    );

    const catalogueProjectId = await resolveCatalogueProjectId(request);
    const models = await readProjectModels(request, catalogueProjectId);
    expect(models.length, 'the catalogue project serves no model').toBeGreaterThan(0);
    const publicModel = models[0];
    const privateModel = await resolvePrivateModel(request);

    const parent = await createAgentWithVersion(request, uniqueName('parentvalid'), {
      instructions: PASSABLE_INSTRUCTIONS,
      model: { modelName: publicModel.name, modelProjectId: catalogueProjectId },
    });
    const child = await createAgentWithVersion(request, uniqueName('childprivate'), {
      instructions: PASSABLE_INSTRUCTIONS,
      model: { modelName: privateModel.modelName, modelProjectId: privateModel.projectId },
    });
    try {
      const attached = await attachSubAgent(request, parent.versionId, {
        applicationId: child.id,
        versionId: child.versionId,
      });
      expect(attached.ok(), await attached.text()).toBe(true);

      const { status, body } = await validate(request, parent.versionId, uniqueName('ver'));
      expect(status, JSON.stringify(body)).toBe(200);
      const llmFindings = [...(body.critical_issues ?? []), ...(body.warnings ?? [])].filter(
        (finding) => finding.field === 'llm_settings',
      );
      // This is the assertion this case says SHOULD hold (a finding naming the sub-agent's private
      // model) — it does not, which is exactly the gap `test.fail` above documents.
      expect(llmFindings.length).toBeGreaterThan(0);
    } finally {
      await deleteAgent(request, child.id);
      await deleteAgent(request, parent.id);
    }
  });
});
