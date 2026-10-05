import type { components } from '@/api/generated/elitea.gen';
import { parseServerTime } from '@/lib/time';

/**
 * Persisted conversation history → what the thread renders.
 *
 * `message_groups` / `message_items` are not typed by the server contract yet
 * (they arrive as loose JSON), so everything here is read defensively: a
 * field of the wrong type is treated as absent, and item types we cannot show
 * on mobile are skipped and flagged.
 */

type ConversationDetail = components['schemas']['ConversationDetail'];
type Participant = components['schemas']['ConversationParticipant'];

export type ChatRole = 'user' | 'assistant';

export interface ChatMessage {
  /** Stable key: the group uuid, else `g<id>`. */
  key: string;
  groupId: number | null;
  uuid: string | null;
  role: ChatRole;
  /** Display name of the author (agent name; null for the user). */
  author: string | null;
  text: string;
  /** Epoch ms, or null if the server sent nothing parsable. */
  createdAt: number | null;
  /** The server still writes this message (a turn in progress). */
  isStreaming: boolean;
  /** The group had items mobile does not render (attachments, canvases, …). */
  hasUnsupportedItems: boolean;
}

const asRecord = (v: unknown): Record<string, unknown> | null =>
  v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
const asString = (v: unknown): string | null => (typeof v === 'string' ? v : null);
const asInt = (v: unknown): number | null => (typeof v === 'number' && Number.isSafeInteger(v) ? v : null);

export function participantName(participant: Participant | undefined): string | null {
  if (!participant) return null;
  for (const source of [participant.meta, participant.entity_meta]) {
    const name = asString(asRecord(source)?.name);
    if (name) return name;
  }
  return null;
}

export function roleOf(participant: Participant | undefined): ChatRole {
  return participant?.entity_name === 'user' ? 'user' : 'assistant';
}

/** The text of one group: its text items in `order_index` order. */
export function groupText(group: Record<string, unknown>): { text: string; unsupported: boolean } {
  const items = Array.isArray(group.message_items) ? group.message_items : [];
  const ordered = items
    .map(asRecord)
    .filter((item): item is Record<string, unknown> => item !== null)
    .sort((a, b) => (asInt(a.order_index) ?? 0) - (asInt(b.order_index) ?? 0));
  const parts: string[] = [];
  let unsupported = false;
  for (const item of ordered) {
    if (item.item_type === 'text_message') {
      const content = asString(asRecord(item.item_details)?.content);
      if (content) parts.push(content);
    } else {
      unsupported = true;
    }
  }
  if (parts.length === 0 && ordered.length === 0) {
    const content = asString(group.content);
    if (content) parts.push(content);
  }
  return { text: parts.join('\n\n'), unsupported };
}

/** Message groups → chat messages, oldest first. */
export function toChatMessages(detail: Pick<ConversationDetail, 'participants' | 'message_groups'> | undefined): ChatMessage[] {
  if (!detail) return [];
  const participants = new Map<number, Participant>();
  for (const p of Array.isArray(detail.participants) ? detail.participants : []) {
    if (asInt(p?.id) !== null) participants.set(p.id, p);
  }
  const groups = Array.isArray(detail.message_groups) ? detail.message_groups : [];
  const messages: ChatMessage[] = [];
  for (const raw of groups) {
    const group = asRecord(raw);
    if (!group) continue;
    const groupId = asInt(group.id);
    const uuid = asString(group.uuid);
    if (groupId === null && !uuid) continue;
    const author = participants.get(asInt(group.author_participant_id) ?? -1);
    const { text, unsupported } = groupText(group);
    messages.push({
      key: uuid ?? `g${groupId}`,
      groupId,
      uuid,
      role: roleOf(author),
      author: roleOf(author) === 'user' ? null : participantName(author),
      text,
      createdAt: parseServerTime(group.created_at),
      isStreaming: group.is_streaming === true,
      hasUnsupportedItems: unsupported,
    });
  }
  // Oldest first; equal or missing times keep the server's id order.
  return messages
    .map((m, index) => ({ m, index }))
    .sort((a, b) => {
      const ta = a.m.createdAt ?? 0;
      const tb = b.m.createdAt ?? 0;
      if (ta !== tb) return ta - tb;
      return (a.m.groupId ?? a.index) - (b.m.groupId ?? b.index);
    })
    .map(({ m }) => m);
}

/**
 * The single agent participant a mobile send can address: exactly one
 * `application` participant. Multi-agent conversations are read-only here.
 */
export function agentParticipant(detail: Pick<ConversationDetail, 'participants'> | undefined):
  | { ok: true; participantId: number; name: string | null }
  | { ok: false; reason: 'none' | 'several' } {
  const agents = (Array.isArray(detail?.participants) ? detail.participants : []).filter(
    (p) => p?.entity_name === 'application' && asInt(p.id) !== null && p.id > 0,
  );
  if (agents.length === 0) return { ok: false, reason: 'none' };
  if (agents.length > 1) return { ok: false, reason: 'several' };
  return { ok: true, participantId: agents[0].id, name: participantName(agents[0]) };
}

/** What a mobile send addresses in a conversation. */
export type ChatTarget =
  | { ok: true; kind: 'agent'; participantId: number; name: string | null }
  | {
      ok: true;
      kind: 'model';
      /** The conversation's `dummy` (model) participant, or 0 when it has none. */
      participantId: number;
      name: string | null;
      /** The model saved on that participant; null means "the project's default". */
      model: { name: string; projectId: number | null } | null;
    }
  | { ok: false; reason: 'none' | 'several' };

function savedModel(participant: Participant | undefined): { name: string; projectId: number | null } | null {
  const llm = asRecord(asRecord(participant?.entity_settings)?.llm_settings);
  const name = asString(llm?.model_name)?.trim();
  if (!name) return null;
  const projectId = asInt(llm?.model_project_id);
  return { name, projectId: projectId !== null && projectId > 0 ? projectId : null };
}

/**
 * The single agent, or, in a conversation with no agent, its model (a plain
 * model chat: the adhoc contract). A model conversation has at most one
 * `dummy` participant besides its users; anything else (several agents,
 * several models, toolkits, …) is read-only on mobile.
 */
export function chatTarget(detail: Pick<ConversationDetail, 'participants'> | undefined): ChatTarget {
  if (!detail) return { ok: false, reason: 'none' };
  const agent = agentParticipant(detail);
  if (agent.ok) return { ok: true, kind: 'agent', participantId: agent.participantId, name: agent.name };
  if (agent.reason === 'several') return agent;
  const others = (Array.isArray(detail.participants) ? detail.participants : []).filter(
    (p) => p && p.entity_name !== 'user',
  );
  if (others.some((p) => p.entity_name !== 'dummy' || asInt(p.id) === null || p.id <= 0)) {
    return { ok: false, reason: 'none' };
  }
  if (others.length > 1) return { ok: false, reason: 'several' };
  const dummy = others[0];
  return {
    ok: true,
    kind: 'model',
    participantId: dummy?.id ?? 0,
    name: participantName(dummy),
    model: savedModel(dummy),
  };
}
