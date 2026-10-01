/**
 * MCP-via-Chat-Conversation OAuth: the Client Secret field's "New Secret"
 * shortcut, reached from inside a live chat turn — onetest package
 * `mcp-servers`, case ELITEA-0725
 * ([EliteaAI/elitea_issues#5116]"MCP via Chat Conversation: Secret Field
 * Shows 'New Secret' Shortcut").
 *
 * ── What was broken, and what A13/F5 fixed ──────────────────────────────
 * `ChatContinue.tsx` (rendered by `ApplicationAnswer` for a message paused
 * on `mcp_authorization_required`) has ALWAYS accepted a `renderAuthModal`
 * slot for exactly this case — its own module doc says so — but nothing
 * filled it: clicking "Continue (Auth)" called `setShowAuthModal(true)` and
 * rendered NOTHING, because `renderAuthModal` was `undefined` all the way
 * up through `ChatMessageList`/`ApplicationAnswer`. That is now wired
 * (`widgets/chat-box/ui/ChatBoxContinuation.tsx`'s `buildChatBoxContinuationProps`,
 * composed into `ChatBox.tsx`) with the REAL `McpAuthModal`
 * (`features/mcps/ui/McpAuthModal.tsx`), whose Client Secret field is now
 * `shared/ui`'s `SecretField` (mode toggle + saved-secret picker + "Create
 * new secret" shortcut) instead of a plain masked text box — the same #441
 * fix `features/toolkits/ui/form/ToolBase/ToolBaseProperty.renderers.tsx`'s
 * own `SecretFieldInput` leaf already applies to the toolkit CREATE/EDIT
 * form's own Client Secret field (a DIFFERENT screen, DIFFERENT case —
 * `mcps.secret-field.spec.ts`'s ELITEA-0726 — already green against it;
 * this case is specifically the chat-conversation mid-run pause, which had
 * no reachable secret field of ANY shape before this fix).
 *
 * ── Why this cannot be driven end to end here ───────────────────────────
 * Reaching `mcp_authorization_required` needs a REAL agent turn against an
 * MCP toolkit whose OAuth metadata the LLM/tool layer discovers live — the
 * runtime plane this stack does not mount for a send in an
 * already-existing conversation (`chat.management.spec.ts`'s module header,
 * note 1; `chat.versionSwitch.spec.ts`'s own header states the identical
 * constraint for its own case). Per this package's own ground rules, that
 * half is proved by unit tests instead:
 *  - `features/mcps/ui/OAuthFormFields.test.tsx` — the "Create new secret"
 *    shortcut on THIS exact field, end to end against a real `QueryClient` +
 *    MSW (open the picker, see the saved secret + the shortcut, click it,
 *    assert the `/settings/secrets?createSecret=1` navigation);
 *  - `widgets/chat-box/ui/ChatBoxContinuation.test.tsx` — proves
 *    `buildChatBoxContinuationProps`'s `renderAuthModal` actually mounts the
 *    real `McpAuthModal` (not a stub) with the slot props `ChatContinue`
 *    forwards, by rendering it and asserting `mcp-auth-modal` + the
 *    server URL appear.
 *
 * What IS asserted here, live against this stack: the entry point the
 * case's own title names — attaching an MCP toolkit to a chat
 * conversation as a participant — is real and reachable via the REST layer
 * this UI wiring sits on top of, using the same `createMcpConnection`
 * fixture `mcps.oauth.spec.ts` already drives.
 */
import { expect, test } from '@playwright/test';

import {
  API_BASE,
  createConversation,
  createMcpConnection,
  deleteConversation,
  DEFAULT_PROJECT_ID,
} from '../../fixtures/api';

interface ParticipantRow {
  readonly entity_name?: string;
  readonly entity_meta?: { readonly id?: string | number; readonly project_id?: string | number };
  readonly entity_settings?: { readonly toolkit_type?: string };
}

function toolkitIdentities(participants: readonly ParticipantRow[]) {
  return participants.filter((participant) => participant.entity_name === 'toolkit').map((participant) => ({
    id: String(participant.entity_meta?.id),
    projectId: String(participant.entity_meta?.project_id),
    type: participant.entity_settings?.toolkit_type,
  }));
}

test('ELITEA-0725: an MCP toolkit attaches to a chat conversation as a participant', async ({ page }) => {
  const mcp = await createMcpConnection(page, DEFAULT_PROJECT_ID, `autotest-mcp-chat-secret-${Date.now()}`, {
    url: 'https://example.invalid/mcp',
  });
  const conversationId = await createConversation(page.request, `autotest-mcp-chat-secret-conv-${Date.now()}`);
  try {
    const attachResponse = await page.request.post(
      `${API_BASE}/elitea_core/participants/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
      {
        data: [
          {
            entity_name: 'toolkit',
            entity_meta: { id: mcp.id, project_id: DEFAULT_PROJECT_ID, name: 'mcp-secret-field-e2e' },
            entity_settings: { toolkit_type: 'mcp' },
          },
        ],
      },
    );
    expect(
      attachResponse.status(),
      `attaching an MCP toolkit as a chat participant must succeed: ${(await attachResponse.text()).slice(0, 300)}`,
    ).toBe(200);

    const attached = (await attachResponse.json()) as readonly ParticipantRow[];
    const expectedToolkit = [{ id: mcp.id, projectId: DEFAULT_PROJECT_ID, type: 'mcp' }];
    expect(attached.map((participant) => participant.entity_name).sort()).toEqual(['dummy', 'toolkit', 'user']);
    expect(toolkitIdentities(attached), 'the complete attach response must contain exactly the MCP row it created').toEqual(expectedToolkit);

    const conversationResponse = await page.request.get(
      `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
    );
    expect(conversationResponse.status()).toBe(200);
    const body = (await conversationResponse.json()) as {
      participants?: readonly ParticipantRow[];
    };
    expect(
      toolkitIdentities(body.participants ?? []),
      'the exact MCP identity and type must be readable back from the conversation',
    ).toEqual(expectedToolkit);
  } finally {
    await deleteConversation(page.request, conversationId);
    await page.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${mcp.id}`);
  }
});
