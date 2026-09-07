/**
 * Support Assistant journey helpers.
 *
 * Two jobs live here, and both are API-only (no page, no locator):
 *
 *  1. Write the admin Features section for `support_assistant`
 *     (`PUT /admin/plugin_config_values/administration/support_assistant`,
 *     `services/elitea-main/internal/api/v2/admin/config_schemas.go`). The
 *     body is `{ values: { <key>: <value>, … } }`, keyed by the field's
 *     `key`, not its `path` — this section's keys and paths are the same
 *     string, unlike `branding`'s nested `extra_ui_config.…` paths.
 *
 *     NUMERIC FIELDS REFUSE `null` WITH 400
 *     (`validateFieldValue`: `case "integer", "number":` requires a JSON
 *     number). Send `0` to clear one — never omit it and never send `null`.
 *
 *  2. Read a support conversation back
 *     (`GET /support_assistant/conversation/{uuid}`,
 *     `services/elitea-main/internal/api/v2/supportassistant/conversations.go`)
 *     and answer the one question the journey's test 3 asks: does a stored
 *     message carry the `<support_assistant_context>` fenced block the
 *     Predict route appends to every question
 *     (`supportassistant/predict.go`'s `composeUserInput`)?
 *
 *     The read is POLLED, not read once. The finalising flag is
 *     `message_group.meta.is_error`, written only when a turn's PROJECTION
 *     lands (`internal/db/queries/agent_chat.sql`). An ABSENT key means the
 *     run has not finished — not "no error" — so a single early read can
 *     see a group that exists but has not been flagged either way yet. See
 *     `e2e/fixtures/api.ts`'s `expectStoredAssistantAnswer` for the same
 *     rule, measured, on the general chat surface.
 */
import type { APIRequestContext } from '@playwright/test';
import { expect } from '@playwright/test';

import { API_BASE } from '../../fixtures/api';

const CONFIG_SECTION_URL = `${API_BASE}/admin/plugin_config_values/administration/support_assistant`;

/** Every key `config_schemas.go`'s `supportAssistantSection` declares. */
export interface SupportAssistantValues {
  readonly support_assistant_enabled: boolean;
  readonly support_project_id: number;
  readonly support_agent_project_id: number;
  readonly support_agent_id: number;
  readonly support_welcome_message: string;
  readonly support_assistant_name: string;
  readonly support_placeholder: string;
}

/**
 * Write the support assistant admin section.
 *
 * Every field is sent on every call, never a partial patch. The store upserts
 * only the keys it is given, so a caller that omits a key keeps whatever a
 * PREVIOUS run left there — the opposite of what a seed step must guarantee.
 */
export async function putSupportAssistantValues(
  api: APIRequestContext,
  values: SupportAssistantValues,
): Promise<void> {
  const response = await api.put(CONFIG_SECTION_URL, { data: { values } });
  expect(
    response.ok(),
    `PUT ${CONFIG_SECTION_URL} -> ${response.status()}: ${(await response.text()).slice(0, 300)}`,
  ).toBe(true);
}

/**
 * Turn the assistant OFF, and clear its numeric fields.
 *
 * `0`, never `null` — the section's own validator refuses a `null` on an
 * `integer` field with 400 (see the file header).
 */
export async function disableSupportAssistant(api: APIRequestContext): Promise<void> {
  await putSupportAssistantValues(api, {
    support_assistant_enabled: false,
    support_project_id: 0,
    support_agent_project_id: 0,
    support_agent_id: 0,
    support_welcome_message: 'Hello! How can I help you today?',
    support_assistant_name: 'ELITEA Support',
    support_placeholder: 'Type a message...',
  });
}

export interface EnableSupportAssistantOptions {
  /**
   * The project the agent lives in, AND the project support conversations
   * live in — the SAME project.
   *
   * A support turn resolves `application_versions` from the support
   * project's own tenant schema (`ResolveCurrentApplicationTurn`), and
   * requires the agent participant's `entity_meta.project_id` to equal it.
   * Pointing `support_agent_project_id` at a project the agent does not
   * live in makes the facade refuse with 503, by design.
   */
  readonly projectId: number;
  readonly agentId: number;
  readonly name: string;
  readonly welcomeMessage: string;
  readonly placeholder: string;
}

/** Turn the assistant ON, pointed at one agent. */
export async function enableSupportAssistant(
  api: APIRequestContext,
  options: EnableSupportAssistantOptions,
): Promise<void> {
  await putSupportAssistantValues(api, {
    support_assistant_enabled: true,
    support_project_id: options.projectId,
    support_agent_project_id: options.projectId,
    support_agent_id: options.agentId,
    support_welcome_message: options.welcomeMessage,
    support_assistant_name: options.name,
    support_placeholder: options.placeholder,
  });
}

/** One `chat_message_items` row, as `ListMessageGroups` shapes it. */
interface RawMessageItem {
  readonly item_type?: string;
  readonly item_details?: { readonly content?: string };
}

/** One `chat_message_group` row, as `ListMessageGroups` shapes it. */
interface RawMessageGroup {
  readonly meta?: Record<string, unknown>;
  readonly message_items?: readonly RawMessageItem[];
}

/** `GET /support_assistant/conversation/{uuid}`'s body. */
export interface RawConversationDetails {
  readonly message_groups?: readonly RawMessageGroup[];
}

/** Every text item's stored content, across every group, in API order. */
function textContents(groups: readonly RawMessageGroup[]): string[] {
  const contents: string[] = [];
  for (const group of groups) {
    for (const item of group.message_items ?? []) {
      if (item.item_type === 'text_message' && item.item_details?.content) {
        contents.push(item.item_details.content);
      }
    }
  }
  return contents;
}

/** True once some group in this conversation carries a finalising flag. */
function turnHasSettled(details: RawConversationDetails): boolean {
  return (details.message_groups ?? []).some(
    (group) => group.meta !== undefined && 'is_error' in group.meta,
  );
}

/**
 * Read one support conversation back, and wait for its turn to SETTLE — a
 * message group whose `meta` carries the `is_error` key, present at all,
 * not merely a non-empty reply. See the file header for why an absent key
 * is "not finished" rather than "not an error".
 */
export async function waitForSupportTurnSettled(
  api: APIRequestContext,
  conversationUuid: string,
  options: { readonly timeout?: number } = {},
): Promise<RawConversationDetails> {
  const { timeout = 60_000 } = options;
  let last: RawConversationDetails = {};

  await expect
    .poll(
      async () => {
        const response = await api.get(`${API_BASE}/support_assistant/conversation/${conversationUuid}`);
        if (!response.ok()) return false;
        last = (await response.json()) as RawConversationDetails;
        return turnHasSettled(last);
      },
      { timeout, message: 'the support turn was never finalised in the store' },
    )
    .toBe(true);

  return last;
}

/**
 * Does some stored message carry the fenced page-context block the widget
 * appends to every question (`composeUserInput` in `predict.go`)?
 */
export function hasFencedSupportContext(details: RawConversationDetails): boolean {
  return textContents(details.message_groups ?? []).some((content) =>
    content.includes('<support_assistant_context>'),
  );
}
