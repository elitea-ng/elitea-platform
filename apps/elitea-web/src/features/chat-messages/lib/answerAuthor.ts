/**
 * Who wrote an answer, as the transcript shows it (ADR-0050 decisions 6 and
 * 7e, client contract 1.38): the participant id the avatar state is keyed
 * by, the message class, the persona's name and role, and the agent's icon.
 *
 * The class comes from `@elitea/client-core/conversation`, so web, desktop
 * and mobile apply one rule.
 */
import { avatarPackOf, classifyMessage, participantKindOf, type AvatarPack, type MessageClass } from '@elitea/client-core/conversation';

import type { MessageGroupWire, MessageParticipantWire } from '@/entities/message/lib/wire';

export interface AnswerAuthor {
  readonly authorParticipantId?: number;
  readonly messageClass: MessageClass;
  readonly personaName?: string;
  readonly authorRole?: string;
  readonly authorIconUrl?: string;
  /** The author's avatar pack (ADR-0050 decision 6); its slots win over `authorIconUrl`. */
  readonly authorAvatarPack?: AvatarPack;
  /** The answering agent's application id (`entity_meta.id` of an `application` participant): its memory's key (ADR-0050 1b). */
  readonly authorApplicationId?: number;
  /** The names of the participants the row is addressed to (`addressed_to`), for a side-talk label (ADR-0050 7e). */
  readonly addresseeNames?: readonly string[];
}

/** The name a participant shows in the transcript: its persona's, else its own. */
function participantDisplayName(participant: MessageParticipantWire | undefined): string {
  return (
    trimmed(participant?.persona?.display_name) ??
    trimmed(participant?.entity_meta?.name) ??
    trimmed(participant?.meta?.name) ??
    trimmed(participant?.meta?.user_name) ??
    ''
  );
}

function addresseeNamesOf(addressed: readonly unknown[] | undefined, participants: readonly MessageParticipantWire[]): string[] {
  if (!Array.isArray(addressed)) return [];
  return addressed
    .map((id) => participantIdOf(id))
    .map((id) => (id === undefined ? '' : participantDisplayName(participants.find((row) => participantIdOf(row.id) === id))))
    .filter((name) => name !== '');
}

function trimmed(value: unknown): string | undefined {
  if (typeof value !== 'string') return undefined;
  const text = value.trim();
  return text === '' ? undefined : text;
}

/** A positive safe integer from a number or a digit string. */
function participantIdOf(value: unknown): number | undefined {
  const id = typeof value === 'string' && /^\d{1,15}$/.test(value) ? Number(value) : value;
  return typeof id === 'number' && Number.isSafeInteger(id) && id > 0 ? id : undefined;
}

/**
 * The icon URL a participant carries: the version's `icon_meta.url`, copied
 * when the agent was added (`entity_settings` first, as `useParticipantEntityIcon`
 * reads it). Only a same-origin path is used: Main serves every agent icon
 * from its own origin (uploads and `/app/default_entity_icons/`), and a
 * third-party URL in the stored meta would make every reader's browser fetch
 * it each time the transcript renders.
 */
export function participantIconUrl(participant: MessageParticipantWire | undefined): string | undefined {
  for (const source of [participant?.entity_settings?.icon_meta, participant?.meta?.icon_meta]) {
    if (source === null || typeof source !== 'object') continue;
    const url = trimmed((source as { readonly url?: unknown }).url);
    if (url !== undefined && url.startsWith('/') && !url.startsWith('//') && !url.startsWith('/\\')) return url;
  }
  return undefined;
}

/**
 * The agent (application id) behind participant `participantId`, whose
 * memory a "remembered" chip addresses (ADR-0050 decision 1b); `undefined`
 * for a person, a system row or an unknown participant.
 */
export function participantApplicationId(participants: readonly MessageParticipantWire[], participantId: unknown): number | undefined {
  const id = participantIdOf(participantId);
  const row = id === undefined ? undefined : participants.find((candidate) => participantIdOf(candidate.id) === id);
  return row?.entity_name === 'application' ? participantIdOf(row.entity_meta?.id) : undefined;
}

/** The author view of one answer row. */
export function answerAuthor(group: MessageGroupWire, participants: readonly MessageParticipantWire[]): AnswerAuthor {
  const authorParticipantId = participantIdOf(group.author_participant_id);
  const author = authorParticipantId === undefined ? undefined : participants.find((row) => participantIdOf(row.id) === authorParticipantId);
  const kinds = new Map<number, string | undefined>();
  for (const row of participants) {
    const id = participantIdOf(row.id);
    if (id !== undefined) kinds.set(id, row.kind);
  }
  const messageClass = classifyMessage({
    authorKind: group.author_kind,
    fallbackRole: 'assistant',
    addressedTo: group.addressed_to,
    visibility: group.visibility,
    cause: group.cause,
    kindOfParticipant: (id) => participantKindOf(kinds.get(id)),
  });
  const personaName = trimmed(author?.persona?.display_name);
  const authorRole = trimmed(author?.persona?.role);
  const authorIconUrl = participantIconUrl(author);
  // Contract 1.49 serves the pack as `avatar_pack_v1`; a 1.46 to 1.48 server sent it as `avatar_pack`.
  const authorAvatarPack = avatarPackOf(author?.persona?.avatar_pack_v1 ?? author?.persona?.avatar_pack);
  const authorApplicationId = participantApplicationId(participants, authorParticipantId);
  const addresseeNames = addresseeNamesOf(group.addressed_to, participants);
  return {
    messageClass,
    ...(authorParticipantId !== undefined ? { authorParticipantId } : {}),
    ...(personaName !== undefined ? { personaName } : {}),
    ...(authorRole !== undefined ? { authorRole } : {}),
    ...(authorIconUrl !== undefined ? { authorIconUrl } : {}),
    ...(authorAvatarPack !== undefined ? { authorAvatarPack } : {}),
    ...(authorApplicationId !== undefined ? { authorApplicationId } : {}),
    ...(addresseeNames.length > 0 ? { addresseeNames } : {}),
  };
}
