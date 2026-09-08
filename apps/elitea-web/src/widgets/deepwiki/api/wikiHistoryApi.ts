/**
 * Reading a wiki chat back from the server.
 *
 * The drawer's conversation used to live in `localStorage` alone
 * (`../lib/chatStorage.ts`), which meant it was gone on another device, in
 * another browser and on a cleared profile. elitea-main now writes both turns
 * of every wiki chat into the ordinary tenant chat tables — `source =
 * 'deepwiki'`, a participant of `entity_name = 'toolkit'` — so what is left
 * for the browser is to READ them. It writes nothing: a client-authorable
 * assistant turn would be forgery, and there is no route to do it with.
 *
 * NO NEW ENDPOINT. Both calls below are the conversation routes the chat page
 * already uses; the listing simply asks two more questions of them —
 * `hidden=only`, because a wiki chat is filed hidden precisely so it does not
 * surface in the ordinary chat list, and `mine=true`, because that listing has
 * never read `is_private` and one member must not read another's questions.
 *
 * The routes are hand-written on the Go side and carry no OpenAPI schema, so
 * these are `eliteaFetch` calls in the shape `entities/conversation`'s own
 * `conversationApi.ts` established — and, like it, they go through
 * `shared/api/unwrap`, because `eliteaFetch` resolves the transport ENVELOPE
 * and reading a field off it yields `undefined` on a 200 (issue #132, twice).
 *
 * NEITHER CALL ADDS A `endpoints.manifest.json` ENTRY, and that is deliberate
 * rather than an omission. The transcript read is the route
 * `toolkits.getIndexHistoryConversationDetails` already registers, so this
 * slice is added to that entry's `usedBy` and nothing else changes. The
 * LISTING has no entry at all: registering a new hand-written id makes
 * `TestSpecRouterConformance/manifest_reverse_check` demand spec coverage,
 * its allowlist "may only shrink" and pins its own size, and describing the
 * conversation routes in `api/v2.yaml` trips six separate gates including two
 * codegens. This adds no ROUTE — `GET /elitea_core/conversations/prompt_lib/…`
 * is one the server already serves — only two more query parameters on it.
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody, unwrapListPage } from '@/shared/api/unwrap';
import type { ChatCapability, ChatMessage } from '@/features/wiki-chat';

/** How many of a toolkit's conversations the drawer asks for. */
const CONVERSATION_PAGE_SIZE = 20;

/**
 * How many message groups one transcript is read with.
 *
 * `sort_order=asc` matters as much as the limit: the route defaults to
 * newest-first, and a transcript rendered in that order reads backwards.
 */
const TRANSCRIPT_LIMIT = 200;

/**
 * One of a toolkit's stored wiki conversations.
 *
 * NOT exported. The drawer names none of these fields in a type of its own —
 * it reads the array `listWikiConversations` returns — and an exported type
 * nothing imports is what `knip` is for.
 */
interface WikiConversationSummary {
  readonly id: string;
  readonly name: string;
  readonly updatedAt: string | undefined;
  /** The browser-side key this conversation was opened under, when it has one. */
  readonly chatKey: string | undefined;
}

interface ConversationRow {
  readonly id?: string | number;
  readonly name?: unknown;
  readonly updated_at?: unknown;
  readonly meta?: { readonly wiki_chat_key?: unknown } | undefined;
}

/**
 * This toolkit's wiki conversations for the signed-in user, newest first.
 *
 * `entity_meta_id` is what scopes the list to ONE wiki: the conversation's
 * `meta.single_participant.entity_meta.id` is the toolkit row, written by the
 * server when the conversation was opened.
 */
export async function listWikiConversations(
  projectId: number | string,
  toolkitId: number | string,
): Promise<readonly WikiConversationSummary[]> {
  const query = new URLSearchParams({
    source: 'deepwiki',
    entity_name: 'toolkit',
    entity_meta_id: String(toolkitId),
    hidden: 'only',
    mine: 'true',
    limit: String(CONVERSATION_PAGE_SIZE),
  });
  const response = await eliteaFetch(
    `/elitea_core/conversations/prompt_lib/${String(projectId)}?${query.toString()}`,
  );
  const { rows } = unwrapListPage<ConversationRow>(response, 'deepwiki wiki conversations');
  return rows.map((row) => ({
    id: String(row.id ?? ''),
    name: typeof row.name === 'string' ? row.name : '',
    updatedAt: typeof row.updated_at === 'string' ? row.updated_at : undefined,
    chatKey: typeof row.meta?.wiki_chat_key === 'string' ? row.meta.wiki_chat_key : undefined,
  }));
}

/** A message group as the conversation-details route embeds it. */
interface MessageGroupWire {
  readonly reply_to_id?: unknown;
  readonly meta?: { readonly capability?: unknown; readonly is_error?: unknown } | undefined;
  readonly message_items?: readonly {
    readonly item_type?: unknown;
    readonly item_details?: { readonly content?: unknown } | undefined;
  }[];
}

/**
 * One conversation's turns, in the drawer's own message shape.
 *
 * The mapping is deliberately narrow. A stored wiki turn is one text item and
 * nothing else — no attachments, no canvas — so anything richer arriving here
 * is a group this drawer cannot render, and it is DROPPED rather than rendered
 * as an empty bubble.
 *
 * `reply_to_id` is what says which side spoke: the server writes the answer as
 * the reply to its question. Reading the author participant instead would mean
 * resolving the participants list to find out which id is the toolkit, for an
 * answer the group already carries.
 */
export async function loadWikiTranscript(
  projectId: number | string,
  conversationId: string,
): Promise<readonly ChatMessage[]> {
  const query = new URLSearchParams({
    messages_limit: String(TRANSCRIPT_LIMIT),
    sort_order: 'asc',
  });
  const response = await eliteaFetch(
    `/elitea_core/conversation/prompt_lib/${String(projectId)}/${conversationId}?${query.toString()}`,
  );
  const body = unwrapBody(response) as { message_groups?: readonly MessageGroupWire[] } | undefined;
  return messagesFromGroups(body?.message_groups ?? []);
}

function messagesFromGroups(groups: readonly MessageGroupWire[]): readonly ChatMessage[] {
  const messages: ChatMessage[] = [];
  for (const group of groups) {
    const content = textOf(group);
    if (content === '') continue;
    const isAnswer = group.reply_to_id !== undefined && group.reply_to_id !== null;
    if (isAnswer) {
      messages.push({
        role: 'assistant',
        content,
        // `isError` is set only when the server said so. Absence is not a
        // claim, and reading it as one would paint every restored answer red.
        ...(group.meta?.is_error === true ? { isError: true } : {}),
      });
      continue;
    }
    const capability = capabilityOf(group);
    messages.push({
      role: 'user',
      content,
      ...(capability ? { capability } : {}),
    });
  }
  return messages;
}

function textOf(group: MessageGroupWire): string {
  for (const item of group.message_items ?? []) {
    if (item.item_type !== 'text_message') continue;
    const content = item.item_details?.content;
    if (typeof content === 'string' && content !== '') return content;
  }
  return '';
}

function capabilityOf(group: MessageGroupWire): ChatCapability | undefined {
  const capability = group.meta?.capability;
  return capability === 'ask' || capability === 'research' ? capability : undefined;
}
