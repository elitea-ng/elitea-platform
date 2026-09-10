/**
 * `conversation_starters` typing — what the CREATE/UPDATE APIs do with a
 * non-string entry, and what the AGENT EDITOR does when one is already
 * stored (onetest wave-1 agents package: ELITEA-0090, ELITEA-0092,
 * ELITEA-0093, ELITEA-0094).
 *
 * ── What the code actually does (read before writing a single assertion) ──
 *
 * `services/elitea-main/internal/api/generated/api.gen.go`'s own doc comment
 * on the `ConversationStarters` field says it plainly: "NOTE(W2): opaque
 * jsonb array round-trip … the store/return round-trip itself is untyped."
 * The domain type is `[]any` (`internal/domain/applications/types.go:82`),
 * and neither the create nor the update handler
 * (`internal/api/v2/applications/handler.go`) rejects a non-string element —
 * it is written to the `jsonb` column and read back exactly as it arrived.
 * ELITEA-0090/0093 expect a 400 here; the server answers 201/200 instead.
 * That is a real gap, not a test-authoring one — verified by reading the
 * handler and the generated model together, not assumed from the hint
 * sheet's "NA-SUSPECT | api-unit-test" (which was about to skip it as
 * out-of-scope; the validation IS missing, which is the more useful finding
 * to record).
 *
 * The FRONTEND side is not broken by this: `ConversationStartersEditor.tsx`
 * coerces every entry through `toString()`
 * (`features/agents/lib/helpers/conversationStarters.helpers.ts`), so a
 * stored `null`/`42`/`true`/`{}` renders as `''`/`'42'`/`'true'`/
 * `'[object Object]'` rather than throwing — ELITEA-0092's UI half is
 * already true, and this file proves it against a REAL agent seeded with
 * exactly the values the case names.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createAgent, deleteAgent } from '../../fixtures/api';

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}cs-validation-${stem}-${String(Date.now()).slice(-7)}`;
}

/*
 * ELITEA-0092 — an agent seeded (directly at the store, bypassing any client
 * validation) with the exact repro payload from the case must open without a
 * full-page crash or a `s.trim is not a function` console error, and the
 * valid string among the four entries must still be visible.
 */
test('J14c-cs: an agent with pre-existing non-string conversation_starters opens without crashing', async ({
  page,
  request,
}) => {
  const name = uniqueName('malformed');
  const agent = await createAgent(request, name);
  try {
    // The malformed values are written through the SAME version-write route
    // the UI itself uses — this is what "pre-existing before the fix, or
    // pre-seeded directly into the database" means operationally against a
    // stack with no direct DB access from the test runner.
    const putResponse = await request.put(
      `${API_BASE}/elitea_core/version/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.id}/${agent.versionId}`,
      { data: { conversation_starters: [null, 42, {}, 'valid starter e2e'] } },
    );
    expect(putResponse.ok(), `seeding malformed conversation_starters must itself succeed (proves the gap)`).toBe(
      true,
    );

    const consoleErrors: string[] = [];
    page.on('pageerror', (error) => consoleErrors.push(String(error)));
    page.on('console', (message) => {
      if (message.type() === 'error') consoleErrors.push(message.text());
    });

    await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
    const panel = page.getByTestId('edit-application-configuration-tab-panel');
    await expect(panel, 'the page must not go blank/white-screen on malformed data').toBeVisible({
      timeout: 20_000,
    });

    // Other sections stay interactive — the crash this case guards against
    // is a full-page one, so the name field is the cheapest proof the rest
    // of the form is still alive.
    await expect(panel.getByTestId('agent-name-input')).toHaveValue(name, { timeout: 20_000 });

    // The valid string among the four malformed entries must still render.
    await expect(panel.getByTestId('agent-conversation-starter-input').last()).toHaveValue('valid starter e2e', {
      timeout: 10_000,
    });

    expect(
      consoleErrors.filter((text) => /\.trim is not a function|TypeError/i.test(text)),
      `no TypeError/trim crash may reach the console: ${consoleErrors.join(' | ')}`,
    ).toEqual([]);
  } finally {
    await deleteAgent(request, agent.id);
  }
});

/*
 * ELITEA-0094 (regression) — valid, string-only conversation_starters
 * continue to be accepted and round-trip intact. Not a dedicated new
 * mechanism: `agents.editor.spec.ts`'s `createAgentWithStarters` and this
 * very file's malformed-seed request above (whose one valid entry survives)
 * already exercise the positive path on every run; this is the direct,
 * minimal proof against the CREATE route itself.
 */
test('J14c-cs: the create API accepts and returns string-only conversation_starters unchanged', async ({
  request,
}) => {
  const name = uniqueName('valid-strings');
  const starters = ['How can you help me?', 'Summarize this document', 'What are your capabilities?'];
  const response = await request.post(`${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name,
      description: `${AUTOTEST_PREFIX}valid starters regression`,
      type: 'agent',
      versions: [{ name: 'base', agent_type: 'openai', instructions: 'hi', conversation_starters: starters }],
    },
  });
  expect(response.status(), 'valid string starters must be accepted').toBeLessThan(300);
  const body = await response.json();
  try {
    expect(body?.version_details?.conversation_starters).toEqual(starters);
  } finally {
    await deleteAgent(request, String(body.id));
  }
});

/*
 * ELITEA-0090 — product gap. The create API SHOULD refuse a
 * `conversation_starters` array carrying non-string entries; it accepts and
 * persists them, per the handler read cited in this file's module doc
 * comment.
 */
test('J14c-cs: [PRODUCT GAP] the create API rejects non-string conversation_starters entries', async ({
  request,
}) => {
  test.fail(true, 'ELITEA-0090: product gap — conversation_starters is an untyped jsonb passthrough; no create-time type validation exists');
  const name = uniqueName('reject-create');
  const createdIds: string[] = [];
  try {
    for (const invalid of [[null], [42], [{}], [true], [null, 42, {}, 'valid starter']]) {
      const response = await request.post(`${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}`, {
        data: {
          name: `${name}-${createdIds.length}`,
          description: `${AUTOTEST_PREFIX}invalid starters variant`,
          type: 'agent',
          versions: [{ name: 'base', agent_type: 'openai', instructions: 'hi', conversation_starters: invalid }],
        },
      });
      if (response.ok()) {
        const body = await response.json();
        if (typeof body?.id === 'string') createdIds.push(body.id);
      }
      expect(
        response.status(),
        `POST with conversation_starters=${JSON.stringify(invalid)} must answer 400`,
      ).toBe(400);
    }
  } finally {
    for (const id of createdIds) await deleteAgent(request, id);
  }
});

/*
 * ELITEA-0093 — the same product gap, on the UPDATE (version PUT) route: a
 * non-string entry should be refused, leaving the previously-stored valid
 * value untouched. It is accepted and overwrites it instead.
 */
test('J14c-cs: [PRODUCT GAP] the version-update API rejects non-string conversation_starters entries', async ({
  request,
}) => {
  test.fail(true, 'ELITEA-0093: product gap — the version PUT applies the same untyped jsonb passthrough as create, so a malformed update silently overwrites valid data');
  const name = uniqueName('reject-update');
  const validStarters = ['How can you help me?', 'Summarize this document', 'What are your capabilities?'];
  const agent = await createAgent(request, name);
  try {
    const seed = await request.put(
      `${API_BASE}/elitea_core/version/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.id}/${agent.versionId}`,
      { data: { conversation_starters: validStarters } },
    );
    expect(seed.ok()).toBe(true);

    for (const invalid of [[null], [42], [null, 42, {}]]) {
      const response = await request.put(
        `${API_BASE}/elitea_core/version/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.id}/${agent.versionId}`,
        { data: { conversation_starters: invalid } },
      );
      expect(
        response.status(),
        `PUT with conversation_starters=${JSON.stringify(invalid)} must answer 400`,
      ).toBe(400);
    }

    const after = await request.get(
      `${API_BASE}/elitea_core/version/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.id}/${agent.versionId}`,
    );
    const afterBody = await after.json();
    expect(
      afterBody?.conversation_starters,
      'no refused variant may have overwritten the original valid data',
    ).toEqual(validStarters);
  } finally {
    await deleteAgent(request, agent.id);
  }
});
