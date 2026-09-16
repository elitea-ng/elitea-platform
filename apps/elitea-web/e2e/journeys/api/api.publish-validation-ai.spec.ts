/**
 * Publish validation: the AI step's contract (issue #940 A18 — ELITEA-0146,
 * "AI Validation Uses Project-Level Low-Tier LLM — Broken Agent LLM Does Not
 * Block Validation").
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS FILE CAN PROVE, AND WHAT IT DELIBERATELY DOES NOT
 * ─────────────────────────────────────────────────────────────────────────────
 * `runPublishValidation` used to hard-code `ai_validation_available: false`.
 * It now runs an advisory pass on the PROJECT's low-tier model when one is
 * configured and a gateway is composed, reports the flag honestly, and folds
 * the findings into `recommendations` with `source: "ai"`.
 *
 * THIS STACK HAS NO MODEL PLANE. There is no `LLM_GATEWAY_URL`, so the
 * completer is nil, the option is not applied, and the honest answer is
 * `ai_validation_available: false`. That is exactly what this file asserts —
 * and it asserts the SHAPE around it, which is the half a deployment with a
 * gateway shares with one without:
 *
 *   - the flag is present and is a boolean, not absent and not a string;
 *   - a deployment that cannot run the step reports `false` rather than
 *     omitting the key (an absent key reads to the dialog as "not checked");
 *   - no finding claims `source: "ai"` when the step did not run — the one
 *     way the flag and the findings could disagree;
 *   - the DETERMINISTIC verdict is complete and a token is still issued, which
 *     is ELITEA-0146's actual claim: whatever the model side is doing, it does
 *     not block the validation.
 *
 * THE MODEL-BACKED PATH IS VERIFIED BY GO TESTS ONLY —
 * `services/elitea-main/internal/api/v2/eliteacore/
 * publish_ai_validation_postgres_integration_test.go`, which drives the same
 * route against the real migration corpus with a FAKE model client and covers
 * what no stack without a model plane can: that the project's low-tier model
 * is the one chosen (and not the version's own), that a failing model leaves
 * the verdict and the token untouched, that findings never reach
 * `critical_issues` or `warnings`, and that an unusable answer reports
 * "unavailable" rather than "no findings". Stubbing a model here would assert
 * the stub.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext, APIResponse } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  createAgentWithVersion,
  deleteAgent,
  resolvePublishAuthorProjectId,
} from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

interface ValidationIssue extends Record<string, unknown> {
  readonly field?: string;
  readonly source?: string;
}

interface ValidationResult {
  readonly status?: string;
  readonly critical_issues?: ValidationIssue[];
  readonly warnings?: ValidationIssue[];
  readonly recommendations?: ValidationIssue[];
  readonly counts?: Record<string, number>;
  readonly ai_validation_available?: unknown;
  readonly validation_token?: unknown;
}

function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

function versionName(tag: string): string {
  return `${tag}-${Math.random().toString(36).slice(2, 8)}`;
}

function validate(
  request: APIRequestContext,
  projectId: string,
  versionId: string,
  body: Record<string, unknown>,
): Promise<APIResponse> {
  return request.post(
    `${API_BASE}/elitea_core/publish_validate/prompt_lib/${projectId}/${versionId}`,
    { data: body },
  );
}

/** Instructions past the check's own 50-character floor, so the deterministic half PASSes. */
const QUALITY_INSTRUCTIONS =
  'You are a release notes assistant. Turn the commits, tickets and review notes the user ' +
  'gives you into a short summary that names what changed, who it affects and what is still open.';

/* onetest: ELITEA-0146 — the response contract of the AI validation step. On a deployment with no
 * model plane the flag is a present, honest `false` and no finding claims an AI source, while the
 * deterministic verdict is complete and still issues a publish token — which is the case's own claim
 * that the model side never blocks the check. The model-BACKED behaviour (the project-level low-tier
 * model is the one used, a broken model changes nothing) is verified by
 * publish_ai_validation_postgres_integration_test.go; see this file's header. */
test('PVAI1: publish validation reports the AI step honestly and is not blocked by it', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const agent = await createAgentWithVersion(
    request,
    autotestName('pvai'),
    {
      instructions: QUALITY_INSTRUCTIONS,
      welcomeMessage: 'Send me the commits and I will draft the notes.',
      conversationStarters: ['Summarise this release.', 'What is still open?'],
    },
    projectId,
    'Drafts release notes from commits, tickets and review notes.',
  );

  try {
    const response = await validate(request, projectId, agent.versionId, {
      version_name: versionName('rel'),
    });
    expect(response.status(), (await response.text()).slice(0, 500)).toBe(200);
    const result = (await response.json()) as ValidationResult;

    // The FLAG. Present, and a boolean — not absent, which the publish dialog
    // would render as "the AI step was not part of this result".
    expect(
      Object.prototype.hasOwnProperty.call(result, 'ai_validation_available'),
      `ai_validation_available is missing from ${JSON.stringify(result)}`,
    ).toBe(true);
    expect(typeof result.ai_validation_available).toBe('boolean');
    // This stack composes no gateway, so the honest answer is false.
    expect(
      result.ai_validation_available,
      'this stack has no LLM_GATEWAY_URL, so the AI step cannot have run',
    ).toBe(false);

    // THE CONSISTENCY CHECK. A `false` flag beside an AI-sourced finding would
    // mean one of the two is lying, and an author would act on the finding.
    const everyFinding = [
      ...(result.critical_issues ?? []),
      ...(result.warnings ?? []),
      ...(result.recommendations ?? []),
    ];
    expect(
      everyFinding.filter((item) => item.source === 'ai'),
      'a finding claims an AI source while the flag says the step did not run',
    ).toHaveLength(0);
    // Every finding names its source, so a client can render an advisory
    // remark differently from a rule.
    for (const finding of everyFinding) {
      expect(typeof finding.source, JSON.stringify(finding)).toBe('string');
    }

    // ELITEA-0146's own claim, in the shape this stack can show it: the model
    // side contributed nothing and the deterministic check still reached a
    // verdict AND issued the token the publish route accepts. A step that
    // failed closed would show up here as a missing token.
    expect(result.status).toBe('PASS');
    expect(Array.isArray(result.critical_issues)).toBe(true);
    expect(Array.isArray(result.warnings)).toBe(true);
    expect(Array.isArray(result.recommendations)).toBe(true);
    expect(typeof result.validation_token, JSON.stringify(result)).toBe('string');
    expect(result.counts).toMatchObject({ critical: 0, warnings: 0, suggestions: expect.any(Number) });
    // `suggestions` counts the SAME list the AI findings would join, so it
    // must agree with the list's length — the one place a future AI finding
    // could be appended without being counted.
    expect(result.counts?.['suggestions']).toBe((result.recommendations ?? []).length);
  } finally {
    await deleteAgent(request, agent.id, projectId);
  }
});
