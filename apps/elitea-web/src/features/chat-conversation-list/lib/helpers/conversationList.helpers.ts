/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/conversation-list/lib/
 * helpers/conversationList.helpers.js` (unit C2).
 */
import type { Conversation } from '@/entities/conversation';

/** A single named bucket of conversations (baseline: a `date_groups[]` entry) — generic over the conversation shape and over any extra bucket fields (e.g. `total`/`offset`) a caller's own state tree carries. */
export interface ConversationGroup<TConversation extends { readonly id: string }> {
  readonly name: string;
  readonly conversations: readonly TConversation[];
}

/**
 * `conversationList.helpers.js:3-19` `redistributeConversationsIntoGroups`,
 * ported verbatim: rebuilds each PREVIOUS group's `id -> group name`
 * membership map, then redistributes a freshly-fetched flat conversation
 * list back into that same grouping — an id not seen in `prevGroups` at all
 * defaults into the `'today'` bucket (baseline's own fallback, `:14`).
 * Consumed by a future page-level unit (baseline: `pages/NewChat/
 * NewChat.jsx:141`) — no hook in THIS slice's `lib/hooks/` calls it
 * directly, so it stays generic over the conversation/group shape rather
 * than hard-coding `entities/folder`'s `DateGroup`.
 */
export function redistributeConversationsIntoGroups<
  TConversation extends { readonly id: string },
  TGroup extends ConversationGroup<TConversation>,
>(prevGroups: readonly TGroup[], newFlatConversations: readonly TConversation[]): TGroup[] {
  const idToGroup = new Map<string, string>();
  for (const group of prevGroups) {
    for (const conv of group.conversations) {
      idToGroup.set(conv.id, group.name);
    }
  }

  return prevGroups.map((group) => {
    const conversations = newFlatConversations.filter((conv) => {
      const originalGroup = idToGroup.get(conv.id);
      if (originalGroup !== undefined) return originalGroup === group.name;
      return group.name === 'today' && !idToGroup.has(conv.id);
    });
    return { ...group, conversations };
  });
}

/**
 * ⚠️ NAMING COLLISION, DELIBERATE. `entities/conversation`'s own
 * `sortConversations` (`entities/conversation/model/selectors.ts`) is a
 * DIFFERENT function with a different module — this is a feature-local port
 * of `conversationList.helpers.js:21-38`'s comparator, kept local (not
 * imported from `entities/conversation`) because this file's whole purpose
 * is to be the literal, auditable 1:1 port of the baseline helpers file the
 * C2 brief asks for, byte-for-byte including its exact tie-break asymmetry
 * (see below). The two are NOT guaranteed interchangeable — do not assume
 * changing one updates the other; this one is not re-exported through this
 * slice's barrel under any name that could shadow the entity's.
 *
 * The tie-break logic is intentionally ASYMMETRIC between the two branches:
 *  - SAME id (a playback snapshot vs. the live row it snapshots):
 *    `isPlayback` is checked BEFORE date — a playback row always sorts
 *    first regardless of timestamps.
 *  - DIFFERENT id: date is checked FIRST; `isPlayback` only breaks an exact
 *    date tie.
 * `new Date(x ?? y ?? NaN)` reproduces the baseline's `new Date(x || y)`
 * when BOTH are missing (`new Date(undefined)` → `Invalid Date`, whose `<`/
 * `>` comparisons are always `false` — `new Date(NaN)` is the type-safe
 * equivalent, same "always false, falls through" behaviour) without ever
 * inventing an epoch-0 fallback timestamp that would sort a dateless row as
 * "oldest" (a real behaviour change the naive `?? 0` would have introduced).
 *
 * Split into the 3 helpers below purely to stay under the §3.5 cyclomatic-
 * complexity budget — each still returns a `-1`/`0`/`1` DIRECT COMPARISON
 * (not `entities/conversation/model/selectors.ts`'s subtraction-of-ranks
 * technique): a subtraction of two `Invalid Date`-derived `NaN` timestamps
 * is itself `NaN`, and `Array.prototype.sort`'s spec leaves a `NaN`-
 * returning comparator's behaviour implementation-defined — silently
 * reintroducing the exact "both dates missing" hazard this function's own
 * `?? NaN` choice (above) was written to avoid. Direct `<`/`>` comparisons
 * on `Date` objects stay reliably `false` for `Invalid Date` instead.
 */
function dateOf(conversation: Conversation): Date {
  return new Date(conversation.updatedAt ?? conversation.createdAt ?? NaN);
}

/** `-1` when `a` is the playback row and `b` isn't (playback sorts first), `1` for the reverse, `0` when neither/both are playback. */
function comparePlaybackFirst(a: Conversation, b: Conversation): number {
  if (a.isPlayback === true && b.isPlayback !== true) return -1;
  if (a.isPlayback !== true && b.isPlayback === true) return 1;
  return 0;
}

/** `-1` when `a` is more recent than `b` (descending order), `1` for the reverse, `0` on an exact tie (including both `Invalid Date`). */
function compareDate(a: Conversation, b: Conversation): number {
  const dateA = dateOf(a);
  const dateB = dateOf(b);
  if (dateA > dateB) return -1;
  if (dateA < dateB) return 1;
  return 0;
}

export function sortConversations(conversations: readonly Conversation[]): Conversation[] {
  return [...conversations].sort((a, b) => {
    if (a.id === b.id) {
      const playbackResult = comparePlaybackFirst(a, b);
      return playbackResult !== 0 ? playbackResult : compareDate(a, b);
    }
    const dateResult = compareDate(a, b);
    return dateResult !== 0 ? dateResult : comparePlaybackFirst(a, b);
  });
}

/**
 * Pinned folders first, each side in its current order (a stable partition).
 * Pinning a folder only flipped its flag, so it stayed where it was in the
 * list; the server lists pinned folders first and the local list now agrees
 * with it the moment the pin lands (issue 2031).
 */
export function pinnedFoldersFirst<TFolder extends { readonly isPinned?: boolean | undefined }>(folders: readonly TFolder[]): TFolder[] {
  return [...folders.filter((folder) => folder.isPinned === true), ...folders.filter((folder) => folder.isPinned !== true)];
}
