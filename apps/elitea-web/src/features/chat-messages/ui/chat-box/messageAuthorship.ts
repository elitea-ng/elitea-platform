import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';

import { t } from '@/shared/i18n';


/**
 * The author id a value actually STATES, or `undefined` when it states none.
 *
 * `''` is not an identity: `entities/message/lib/normalise.ts` uses the empty
 * string as "the attribution STOPS here" for a row with no author, and
 * `ChatBox.tsx` passes `userId={userId ?? ''}` while `useGetCurrentAuthor` is
 * still in flight (or forever, if it fails). Both spellings of "unstated"
 * therefore have to collapse to one before any comparison.
 *
 * `String()`-normalised for the same reason `normaliseUserMessage` documents:
 * `message.userId` is always a string, but the reader's id comes from the
 * unvalidated `GET /social/author` envelope, whose `id` the transcript and
 * participants payloads state as a NUMBER.
 */
function statedAuthorId(value: string | undefined): string | undefined {
  if (value === undefined || value === '') return undefined;
  return String(value);
}

/**
 * Whether the reader may act on a message authored by `authorId`.
 *
 * The two unstated sides are deliberately NOT symmetric:
 *
 * - reader unstated ⇒ PERMIT. This is the documented pre-identity escape
 *   hatch — no reader has been stated, so the surface keeps the behaviour it
 *   had before author identity existed rather than refusing everything. It was
 *   unreachable until `statedAuthorId` above, because `ChatBox` renders `''`
 *   rather than `undefined`, so a reader whose author query was merely PENDING
 *   was refused edit and delete on their own messages with no explanation.
 * - author unstated ⇒ REFUSE. An unattributed row must not become editable
 *   and deletable by everyone in a shared conversation; absent attribution is
 *   the one case where refusing is the safe answer.
 */
export function isOwnMessage(readerId: string | undefined, authorId: string | undefined): boolean {
  const reader = statedAuthorId(readerId);
  if (reader === undefined) return true;
  const author = statedAuthorId(authorId);
  if (author === undefined) return false;
  return reader === author;
}

/**
 * Which side a question sits on: someone else's (left) only when BOTH the
 * reader and the author are stated and differ. Layout is not a permission, so
 * an unstated side does not refuse the way `isOwnMessage` must — a reloaded
 * row the server attributed to nobody is far more likely the reader's own
 * question in their own chat than a stranger's, and drawing it on the left
 * would make every such conversation look like someone else's.
 */
export function isOtherAuthor(readerId: string | undefined, authorId: string | undefined): boolean {
  const reader = statedAuthorId(readerId);
  const author = statedAuthorId(authorId);
  return reader !== undefined && author !== undefined && reader !== author;
}

/**
 * The reader's display name for a question that is STATED to be theirs: both
 * ids present and equal. A reloaded row names no author at all, and the reader's
 * name must not stand in for it (a shared conversation's other people would be
 * labelled with the reader), so that case yields nothing and the bubble falls
 * gets no label from here ("You" is only said when the author is known to be the reader).
 */
function statedOwnerCaption(readerId: string | undefined, authorId: string | undefined, readerName: string | undefined): string | undefined {
  const reader = statedAuthorId(readerId);
  if (reader === undefined || reader !== statedAuthorId(authorId)) return undefined;
  return readerName || t('features.chatMessages.ownAuthorFallback', 'You');
}


/**
 * The question as its bubble should caption it: when the list can STATE the
 * question is the reader's, the reader's name (or "You") stands in its `name`;
 * otherwise the row is returned untouched and keeps whatever author it states.
 */
export function captionedForReader(message: ChatMessage, readerId: string | undefined, readerName: string | undefined): ChatMessage {
  const caption = statedOwnerCaption(readerId, message.userId, readerName);
  return caption === undefined ? message : { ...message, name: caption };
}
