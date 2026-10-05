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
  /** The author's participant row id (who answered), when known. */
  authorParticipantId: number | null;
  /**
   * The signed-in user wrote it (drawn as "me"). False for another person in
   * a shared conversation, and for every assistant message.
   */
  mine: boolean;
  /** Display name of the author: the agent, or another person; null for "me". */
  author: string | null;
  text: string;
  /** Epoch ms, or null if the server sent nothing parsable. */
  createdAt: number | null;
  /** The server still writes this message (a turn in progress). */
  isStreaming: boolean;
  /** The group had items mobile does not render (canvases, …). */
  hasUnsupportedItems: boolean;
  /** Files sent with the message (`attachment_message` items). */
  attachments: ChatAttachment[];
  /** Why the answer failed (`meta.is_error`), else null. A failed turn is stored with no text. */
  error: string | null;
}

/** One file of a message. */
export interface ChatAttachment {
  key: string;
  /** The file name shown (the stored name, `{conversation_uuid}/` dropped). */
  name: string;
  /** `/{bucket}/{conversation_uuid}/{name}`, as stored. */
  filepath: string;
  /** The last path segment: what downloadConversationAttachment takes. */
  storedName: string;
  /** The conversation uuid the file is stored under. */
  conversationUuid: string | null;
  kind: 'image' | 'document';
  /**
   * The image the model was given, as a `data:` URL (a thumbnail without a
   * download). Only small PNG/JPEG/GIF/WebP data URLs are kept.
   */
  inlineDataUrl: string | null;
}

/** The longest data URL kept for a thumbnail (it already lives in the encrypted store). */
export const MAX_INLINE_DATA_URL = 256 * 1024;
const DATA_URL_RE = /^data:image\/(png|jpeg|gif|webp);base64,[A-Za-z0-9+/=]+$/;
const UUID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** An `attachment_message` item → what the thread shows, or null when it names no file. */
export function attachmentOf(item: Record<string, unknown>): ChatAttachment | null {
  const details = asRecord(item.item_details);
  const filepath = asString(details?.filepath);
  if (!details || !filepath) return null;
  const segments = filepath.split('/');
  const storedName = segments[segments.length - 1] ?? '';
  if (storedName === '' || storedName === '.' || storedName === '..') return null;
  const conversationUuid = segments.length >= 4 && UUID_RE.test(segments[segments.length - 2]) ? segments[segments.length - 2] : null;
  const named = asString(details.name);
  const name = (named ? named.slice(named.lastIndexOf('/') + 1) : storedName) || storedName;
  let inlineDataUrl: string | null = null;
  for (const chunk of Array.isArray(details.content) ? details.content : []) {
    const url = asString(asRecord(asRecord(chunk)?.image_url)?.url);
    if (url && url.length <= MAX_INLINE_DATA_URL && DATA_URL_RE.test(url)) {
      inlineDataUrl = url;
      break;
    }
  }
  const declared = asString(details.attachment_type);
  const kind = declared === 'image' || inlineDataUrl !== null || /\.(png|jpe?g|gif|webp|heic|heif)$/i.test(storedName) ? 'image' : 'document';
  return {
    key: String(asInt(item.id) ?? asInt(details.id) ?? filepath),
    name,
    filepath,
    storedName,
    conversationUuid,
    kind,
    inlineDataUrl,
  };
}

const asRecord = (v: unknown): Record<string, unknown> | null =>
  v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
const asString = (v: unknown): string | null => (typeof v === 'string' ? v : null);
const asInt = (v: unknown): number | null => (typeof v === 'number' && Number.isSafeInteger(v) ? v : null);

export function participantName(participant: Participant | undefined): string | null {
  if (!participant) return null;
  const meta = asRecord(participant.meta);
  // A user participant's display name is `meta.user_name`; every other kind's is `meta.name`.
  const userName = participant.entity_name === 'user' ? asString(meta?.user_name)?.trim() : null;
  if (userName) return userName;
  for (const source of [participant.meta, participant.entity_meta]) {
    const name = asString(asRecord(source)?.name);
    if (name) return name;
  }
  return null;
}

/** A user participant's USER id (`entity_meta.id`), never the participant row id. */
export function participantUserId(participant: Participant | undefined): number | null {
  if (participant?.entity_name !== 'user') return null;
  const id = asInt(asRecord(participant.entity_meta)?.id);
  return id !== null && id > 0 ? id : null;
}

/** Who "me" is, for deciding which user messages are mine. */
export interface SelfContext {
  /** The signed-in user's USER id, when known. */
  selfUserId: number | null;
  /** True when this device sent the question with this uuid. */
  isSentQuestion?: (uuid: string) => boolean;
}

/**
 * The signed-in user's id as this transcript proves it: the author of a user
 * message group this device sent. Null when nothing proves it.
 */
export function inferSelfUserId(
  detail: Pick<ConversationDetail, 'participants' | 'message_groups'> | undefined,
  isSentQuestion: (uuid: string) => boolean,
): number | null {
  if (!detail) return null;
  const byId = new Map<number, Participant>();
  for (const p of Array.isArray(detail.participants) ? detail.participants : []) {
    if (asInt(p?.id) !== null) byId.set(p.id, p);
  }
  for (const raw of Array.isArray(detail.message_groups) ? detail.message_groups : []) {
    const group = asRecord(raw);
    const uuid = asString(group?.uuid);
    if (!group || !uuid || !isSentQuestion(uuid)) continue;
    const id = participantUserId(byId.get(asInt(group.author_participant_id) ?? -1));
    if (id !== null) return id;
  }
  return null;
}

export function roleOf(participant: Participant | undefined): ChatRole {
  return participant?.entity_name === 'user' ? 'user' : 'assistant';
}

/** The text of one group: its text items in `order_index` order. */
export function groupText(group: Record<string, unknown>): { text: string; unsupported: boolean; attachments: ChatAttachment[] } {
  const items = Array.isArray(group.message_items) ? group.message_items : [];
  const ordered = items
    .map(asRecord)
    .filter((item): item is Record<string, unknown> => item !== null)
    .sort((a, b) => (asInt(a.order_index) ?? 0) - (asInt(b.order_index) ?? 0));
  const parts: string[] = [];
  const attachments: ChatAttachment[] = [];
  let unsupported = false;
  for (const item of ordered) {
    if (item.item_type === 'text_message') {
      const content = asString(asRecord(item.item_details)?.content);
      if (content) parts.push(content);
    } else if (item.item_type === 'attachment_message') {
      const attachment = attachmentOf(item);
      if (attachment) attachments.push(attachment);
      else unsupported = true;
    } else {
      unsupported = true;
    }
  }
  if (parts.length === 0 && ordered.every((item) => item.item_type !== 'text_message')) {
    const content = asString(group.content);
    if (content) parts.push(content);
  }
  return { text: parts.join('\n\n'), unsupported, attachments };
}

/**
 * Is a user message mine? By user id when "me" is known; else when this
 * device sent it; else when the conversation has only one person (it can
 * only be me). In a shared conversation with "me" still unknown the message
 * is drawn on my side but named, so nobody else's words read as mine.
 */
function authorship(
  author: Participant | undefined,
  uuid: string | null,
  people: number,
  self: SelfContext,
): { mine: boolean; named: boolean } {
  if (self.selfUserId !== null) {
    const mine = participantUserId(author) === self.selfUserId;
    return { mine, named: !mine };
  }
  if (uuid !== null && self.isSentQuestion?.(uuid)) return { mine: true, named: false };
  if (people <= 1) return { mine: true, named: false };
  return { mine: true, named: true };
}

/** Message groups → chat messages, oldest first. */
/** The failure a group records: `meta` on the conversation read, `metadata` on the messages route. */
function groupError(group: Record<string, unknown>): string | null {
  const meta = asRecord(group.meta) ?? asRecord(group.metadata);
  if (meta?.is_error !== true) return null;
  const reason = typeof meta.error === 'string' ? meta.error.trim() : '';
  return reason !== '' ? reason.slice(0, 500) : 'This answer failed.';
}

export function toChatMessages(
  detail: Pick<ConversationDetail, 'participants' | 'message_groups'> | undefined,
  self: SelfContext = { selfUserId: null },
): ChatMessage[] {
  if (!detail) return [];
  const participants = new Map<number, Participant>();
  for (const p of Array.isArray(detail.participants) ? detail.participants : []) {
    if (asInt(p?.id) !== null) participants.set(p.id, p);
  }
  const people = [...participants.values()].filter((p) => p.entity_name === 'user').length;
  const groups = Array.isArray(detail.message_groups) ? detail.message_groups : [];
  const messages: ChatMessage[] = [];
  for (const raw of groups) {
    const group = asRecord(raw);
    if (!group) continue;
    const groupId = asInt(group.id);
    const uuid = asString(group.uuid);
    if (groupId === null && !uuid) continue;
    const author = participants.get(asInt(group.author_participant_id) ?? -1);
    const { text, unsupported, attachments } = groupText(group);
    const role = roleOf(author);
    const who = role === 'user' ? authorship(author, uuid, people, self) : { mine: false, named: true };
    messages.push({
      key: uuid ?? `g${groupId}`,
      groupId,
      uuid,
      role,
      authorParticipantId: asInt(group.author_participant_id),
      mine: who.mine,
      author: who.named ? participantName(author) : null,
      text,
      createdAt: parseServerTime(group.created_at),
      isStreaming: group.is_streaming === true,
      hasUnsupportedItems: unsupported,
      attachments,
      error: groupError(group),
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

/** An agent (or pipeline) participant a send can address. */
export interface AgentOption {
  participantId: number;
  name: string | null;
  /** The application id (`entity_meta.id`). */
  agentId: number | null;
  /** The application's project (`entity_meta.project_id`). */
  agentProjectId: number | null;
  /** The pinned version (`entity_settings.version_id`). */
  versionId: number | null;
}

const AGENT_KINDS = new Set(['application', 'pipeline']);

/** The conversation's agents and pipelines, in participant order. */
export function agentOptions(detail: Pick<ConversationDetail, 'participants'> | undefined): AgentOption[] {
  return (Array.isArray(detail?.participants) ? detail.participants : [])
    .filter((p) => p && AGENT_KINDS.has(p.entity_name) && asInt(p.id) !== null && p.id > 0)
    .map((p) => {
      const meta = asRecord(p.entity_meta);
      const agentId = asInt(meta?.id);
      const agentProjectId = asInt(meta?.project_id);
      const versionId = asInt(asRecord(p.entity_settings)?.version_id);
      return {
        participantId: p.id,
        name: participantName(p),
        agentId: agentId !== null && agentId > 0 ? agentId : null,
        agentProjectId: agentProjectId !== null && agentProjectId > 0 ? agentProjectId : null,
        versionId: versionId !== null && versionId > 0 ? versionId : null,
      };
    });
}

/** What a mobile send addresses in a conversation. */
export type ChatTarget =
  | {
      ok: true;
      kind: 'agent';
      participantId: number;
      name: string | null;
      agent: AgentOption;
      /** Every agent the user may address instead (the "To:" chip, when more than one). */
      choices: AgentOption[];
    }
  | {
      ok: true;
      kind: 'model';
      /** The conversation's `dummy` (model) participant, or 0 when it has none. */
      participantId: number;
      name: string | null;
      /** The model saved on that participant; null means "the project's default". */
      model: { name: string; projectId: number | null } | null;
    }
  /**
   * none: nothing mobile can address; several: several models;
   * toolkit_agent: toolkits next to agents (the server refuses that turn).
   */
  | { ok: false; reason: 'none' | 'several' | 'toolkit_agent' };

function savedModel(participant: Participant | undefined): { name: string; projectId: number | null } | null {
  const llm = asRecord(asRecord(participant?.entity_settings)?.llm_settings);
  const name = asString(llm?.model_name)?.trim();
  if (!name) return null;
  const projectId = asInt(llm?.model_project_id);
  return { name, projectId: projectId !== null && projectId > 0 ? projectId : null };
}

/**
 * Whether an agent may be added to the conversation from mobile. Not next to
 * toolkits: the server refuses a turn that mixes them, so adding one would
 * turn a working model chat into one that cannot be messaged.
 */
export function agentsCanJoin(detail: Pick<ConversationDetail, 'participants'> | undefined): boolean {
  if (!detail) return false;
  const participants = Array.isArray(detail.participants) ? detail.participants : [];
  return !participants.some((p) => p?.entity_name === 'toolkit');
}

export interface TargetChoice {
  /** The agent the user picked ("To:" chip or "#"), when still attached. */
  targetParticipantId?: number | null;
  /** The participant that wrote the newest answer. */
  lastAnsweredBy?: number | null;
}

/**
 * What a send addresses:
 * - agents (applications, pipelines): the one picked, else the one that
 *   answered last, else the first; several are fine (the "To:" chip
 *   switches). Toolkits next to agents stay read-only (the server refuses).
 * - no agent: the conversation's model (a plain model chat, adhoc). Toolkit
 *   participants are fine there (they become the model's tools); more than
 *   one model, or a kind mobile does not know, is read-only.
 */
export function chatTarget(detail: Pick<ConversationDetail, 'participants'> | undefined, choice: TargetChoice = {}): ChatTarget {
  if (!detail) return { ok: false, reason: 'none' };
  const participants = (Array.isArray(detail.participants) ? detail.participants : []).filter((p) => p);
  const agents = agentOptions(detail);
  const toolkits = participants.filter((p) => p.entity_name === 'toolkit');
  if (agents.length > 0) {
    if (toolkits.length > 0) return { ok: false, reason: 'toolkit_agent' };
    const pick =
      agents.find((a) => a.participantId === choice.targetParticipantId) ??
      agents.find((a) => a.participantId === choice.lastAnsweredBy) ??
      agents[0];
    return { ok: true, kind: 'agent', participantId: pick.participantId, name: pick.name, agent: pick, choices: agents };
  }
  const others = participants.filter((p) => p.entity_name !== 'user' && p.entity_name !== 'toolkit');
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
