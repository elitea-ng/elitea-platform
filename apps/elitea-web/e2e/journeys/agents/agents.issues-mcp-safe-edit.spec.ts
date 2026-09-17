/**
 * elitea_issues package I1-agents — #6528 "internal MCP `update_version`
 * (put_elitea_core_version) Rejects Non-Instruction Edits Because It
 * Triggers Safety Check".
 *
 * Re-judged: NA / not reproduced, against a narrower claim than the filed
 * one. The internal-MCP tool the report names (`put_elitea_core_version`,
 * an in-chat agent-self-edit tool) does not exist in this Go backend at
 * all — `internal/api/v2/drafts/drafts.go` explicitly documents "EDIT MODE
 * BY ID... is not ported (#254)", and no agent-execution code calls
 * `applications.Handler.UpdateVersion`. What DOES exist, and IS this
 * package's scope, is that same REST endpoint
 * (`PUT /elitea_core/version/prompt_lib/{p}/{app}/{version}`) any client —
 * including a future MCP wrapper — would have to go through. It carries no
 * such safety check: `body["instructions"]` is accepted unconditionally
 * (`applications/handler.go:1156-1159`), with no comparison against the
 * previously-stored value. This journey pins THAT: a version update that
 * carries `instructions` alongside an unrelated field change (matching the
 * exact repro the issue describes — "a small, non-instruction change") is
 * never rejected, and the non-instruction field is the one that actually
 * changes.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createAgentWithVersion,
  deleteAgent,
  readVersion,
} from '../../fixtures/api';

const SUFFIX = '-mcpsafeedit';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}${SUFFIX}`;
}

/* onetest: elitea_issues #6528 — a version PUT carrying `instructions` alongside an unrelated field change is never rejected as an "instructions edit" */
test('J-mcp-safe-edit: updating welcome_message while instructions is present in the body succeeds, unchanged', async ({
  request,
}) => {
  const name = uniqueName('agent');
  const originalInstructions = 'Answer every question about widgets.';
  const agent = await createAgentWithVersion(request, name, {
    instructions: originalInstructions,
    welcomeMessage: 'Hello!',
  });

  try {
    // Mirrors the issue's own repro steps: a small, non-instruction change
    // (`welcome_message`), with `instructions` carried in the payload at its
    // CURRENT, unintentional-edit value — exactly what a tool schema
    // defaulting the field would send.
    const response = await request.put(
      `${API_BASE}/elitea_core/version/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.id}/${agent.versionId}`,
      {
        data: {
          name: 'base',
          instructions: originalInstructions,
          welcome_message: 'Updated welcome message',
        },
      },
    );
    expect(
      response.ok(),
      `no safety check should ever refuse this: ${response.status()} ${(await response.text()).slice(0, 300)}`,
    ).toBe(true);

    const stored = await readVersion(request, agent.id, agent.versionId);
    expect(stored.welcomeMessage).toBe('Updated welcome message');
    expect(stored.instructions).toBe(originalInstructions);
  } finally {
    await deleteAgent(request, agent.id);
  }
});
