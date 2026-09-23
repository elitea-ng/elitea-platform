/**
 * `conversation_starters` typing — what the CREATE/UPDATE APIs do with a
 * non-string entry, and what the AGENT EDITOR does when one is already
 * stored (onetest wave-1 agents package: ELITEA-0090, ELITEA-0092,
 * ELITEA-0093, ELITEA-0094).
 *
 * ── What the code actually does (read before writing a single assertion) ──
 *
 * `services/elitea-main/internal/api/generated/api.gen.go`'s own doc comment
 * on the `ConversationStarters` field still says "NOTE(W2): opaque jsonb
 * array round-trip … the store/return round-trip itself is untyped" — the
 * domain type is `[]any` (`internal/domain/applications/types.go:82`) and
 * the STORAGE stays loosely typed. #896 closed the actual gap at the
 * create/update handlers instead: `validateConversationStarters`
 * (`internal/api/v2/applications/handler.go`) now rejects any
 * `conversation_starters` array carrying a non-string element with 400,
 * on both the create route and the version PUT, before anything is
 * written — so ELITEA-0090/0093 get the 400 they expect and a malformed
 * update can no longer overwrite previously-valid starters.
 *
 * The FRONTEND side is not broken by this: `ConversationStartersEditor.tsx`
 * coerces every entry through `toString()`
 * (`features/agents/lib/helpers/conversationStarters.helpers.ts`), so a
 * stored `null`/`42`/`true`/`{}` renders as `''`/`'42'`/`'true'`/
 * `'[object Object]'` rather than throwing — ELITEA-0092's UI half is
 * already true. #896 fixing the write path means this file can no longer
 * seed a REAL agent with malformed data to prove it end-to-end (see the
 * ELITEA-0092 note below); the coercion itself stays unit-tested.
 */
import { test, expect } from '@playwright/test';

import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createAgent, deleteAgent } from '../../fixtures/api';

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}cs-validation-${stem}-${String(Date.now()).slice(-7)}`;
}

/*
 * ELITEA-0092 — COVERED-EXISTING, not runnable as an E2E case any more.
 *
 * This test used to seed a malformed `conversation_starters` array through
 * `PUT /version/...` (the same route the UI uses) to produce "pre-existing
 * non-string data", since the runner has no direct DB access. #896 closed
 * exactly that write path with a 400 (see the module doc above and
 * ELITEA-0093 below) — so as of this fix there is no longer ANY route on
 * this stack that can persist a non-string entry, and a fresh e2e stack
 * never carries pre-#896 legacy rows. The seed line this test depended on
 * (`putResponse.ok()`) now correctly fails, because the gap it exploited is
 * fixed.
 *
 * The behaviour ELITEA-0092 actually cares about — the agent editor
 * tolerating a non-string entry instead of crashing — is unit-tested
 * directly and does not need a live agent to prove it:
 *   `src/features/agents/lib/helpers/conversationStarters.helpers.test.ts`
 *   (`toString` coerces null/42/objects the same way this case named)
 *   `src/features/agents/ui/ConversationStartersEditor.test.tsx`
 * Both cover the coercion this case describes; nothing here is untested,
 * only unreachable end-to-end post-fix.
 */

/*
 * ELITEA-0094 (regression) — valid, string-only conversation_starters
 * continue to be accepted and round-trip intact. Not a dedicated new
 * mechanism: `agents.editor.spec.ts`'s `createAgentWithStarters` and this
 * very file's malformed-seed request above (whose one valid entry survives)
 * already exercise the positive path on every run; this is the direct,
 * minimal proof against the CREATE route itself.
 */
/* onetest: ELITEA-0094 — the create API accepts and returns string-only conversation_starters unchanged */
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
/* onetest: ELITEA-0090 — the create API rejects non-string conversation_starters entries */
test('J14c-cs: the create API rejects non-string conversation_starters entries', async ({
  request,
}) => {
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
/* onetest: ELITEA-0093 — the version-update API rejects non-string conversation_starters entries */
test('J14c-cs: the version-update API rejects non-string conversation_starters entries', async ({
  request,
}) => {
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
