/**
 * Real data composition for the `/chat` page — the piece `features/chat-
 * messages/index.ts` and `processes/chat/index.ts` both describe as
 * pending ("only real consumer is a not-yet-built app/-level composition
 * root that wires an actual chat page"). Resolves project/user/
 * conversation/active-participant and maps them into the shape `ChatBox`
 * (the C6 composition root) expects.
 *
 * DISCLOSED GAP — `projectId` has no fully-wired source yet. Three
 * candidate sources exist in this codebase today; only the third is live:
 *  - `src/app/router-context.ts`'s `AuthContext.getSelectedProjectId()` is
 *    a stub (`() => undefined`) pending unit R2 — confirmed via
 *    `src/app/App.tsx`'s `<RouterProvider router={router} />`, which never
 *    overrides the router's context with a real implementation.
 *  - `widgets/app-shell`'s `useSelectedProject()` is real and working, but
 *    is only ever populated by a user picking a project through
 *    `AppShell` (unit S1), which no route mounts yet.
 *  - The old app's own fallback — "selected project id, falling back to
 *    `personal_project_id`" (`router-context.ts`'s own doc comment
 *    records this as the old app's `useSelectedProjectId()` behavior) —
 *    IS fully available today via `useGetCurrentAuthor()`, so that
 *    fallback is what this hook actually uses. Once AppShell/S1 lands, a
 *    real `useSelectedProject().project` selection correctly takes
 *    priority over this fallback below, matching old-app precedence.
 */
import { useMemo } from 'react';

import { conversationApi } from '@/entities/conversation';
import type { MessageGroupWire } from '@/entities/message';
import type { SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { unwrapList } from '@/shared/api/unwrap';
import { isLocalWorkConversation, localWorkFolderName } from '@/shared/lib/localWork';
import { useSelectedProject } from '@/widgets/app-shell';
import type { ChatBoxProps } from '@/widgets/chat-box';

const MESSAGE_PAGE_SIZE = 50;

export interface UseChatPageDataParams {
  readonly conversationId: string | undefined;
}

export interface UseChatPageDataResult {
  readonly projectId: string | undefined;
  readonly user: { readonly id: string; readonly name: string; readonly avatar: string } | undefined;
  readonly activeConversation: NonNullable<ChatBoxProps['conversation']>['active'];
  readonly isLoadingConversation: boolean;
  /** Set for a desktop Local work thread (read-only here); its folder's name when recorded. */
  readonly localWork: { readonly folderName: string | undefined } | undefined;
}

/**
 * `getCurrentAuthorResponse`'s `.data` is a `SocialAuthorProfile |
 * N401Response` union (discriminated by `.status`), but `eliteaFetch`
 * throws on any non-2xx response rather than resolving with the error
 * variant (§3.6 unwrap contract) — the 401 branch is declared but
 * unreachable at this read site. Same established precedent as
 * `features/chat-conversation-list/ui/folders/FolderItem.tsx`'s own
 * `useCurrentUserId`.
 */
function currentAuthorOf(data: unknown): SocialAuthorProfile | undefined {
  return (data as { readonly data?: SocialAuthorProfile } | undefined)?.data;
}

/**
 * Adapts Main's current flat message rows to the message-group shape used by
 * the ported chat renderer.
 *
 * Main returns `uid`, `role` and `metadata`. The baseline converter expects
 * `uuid`, reply linkage and `meta`. Keeping this adapter at the API composition
 * boundary avoids teaching every message component both wire formats.
 */
type CurrentMessageRow = Partial<MessageGroupWire> & {
  readonly uid?: unknown;
  readonly role?: unknown;
  readonly metadata?: unknown;
};

function nonEmptyString(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined;
}

function adaptCurrentMessageRow(value: unknown, index: number, previousUserId: string | number | undefined): MessageGroupWire {
  const row = value as CurrentMessageRow;
  const id = row.id ?? String(index);
  const uuid = nonEmptyString(row.uuid) ?? nonEmptyString(row.uid) ?? String(id);
  const role = nonEmptyString(row.role);
  const needsReplyLink = role === 'assistant' && previousUserId !== undefined && row.reply_to_id === undefined;
  return {
    ...row,
    id,
    uuid,
    content: typeof row.content === 'string' ? row.content : '',
    created_at: typeof row.created_at === 'string' ? row.created_at : new Date(0).toISOString(),
    ...(role !== undefined ? { role } : {}),
    ...(row.meta === undefined && row.metadata !== undefined
      ? { meta: row.metadata as MessageGroupWire['meta'] }
      : {}),
    ...(needsReplyLink ? { reply_to_id: previousUserId, question_id: previousUserId } : {}),
  } as MessageGroupWire;
}

function adaptCurrentMessageRows(rows: readonly unknown[]): MessageGroupWire[] {
  let previousUserId: string | number | undefined;
  return rows.map((value, index) => {
    const adapted = adaptCurrentMessageRow(value, index, previousUserId);
    if ((adapted as { readonly role?: string }).role === 'user') previousUserId = adapted.id;
    return adapted;
  });
}

/** Maps the REST conversation-details + message-list queries into `ChatBox`'s `activeConversation` prop shape. */
function useActiveConversation(projectId: string | undefined, conversationId: string | undefined): { readonly activeConversation: NonNullable<ChatBoxProps['conversation']>['active']; readonly isLoading: boolean; readonly localWork: UseChatPageDataResult['localWork'] } {
  const enabled = projectId !== undefined && conversationId !== undefined;
  const detailsQuery = conversationApi.useDetails({ projectId: projectId ?? '', id: conversationId ?? '' }, { enabled });
  const messageListQuery = conversationApi.useMessageList(
    { projectId: projectId ?? '', conversationId: conversationId ?? '', page: 0, pageSize: MESSAGE_PAGE_SIZE, params: { sort_order: 'asc' } },
    { enabled },
  );

  const activeConversation = useMemo<NonNullable<ChatBoxProps['conversation']>['active']>(() => {
    if (!conversationId) return { isNew: true };
    if (!detailsQuery.data) return undefined;
    /*
     * The message list is paginated as {items,total,page,page_size,total_pages}
     * — measured, not assumed. The original unwrap tried `rows` and otherwise
     * fell through to the response itself, so neither branch matched and the
     * ENVELOPE OBJECT was handed to ChatBox as `message_groups`.
     * convertMessagesToChatHistory then did `[...(messageGroups ?? [])]` on a
     * non-iterable and threw, so every deep link to /app/chat/:conversationId
     * hit the route error boundary — while both API calls returned 200, which
     * is why it read as a frontend mystery rather than a shape mismatch.
     *
     * The three-shape unwrap that replaced it now lives in ONE place
     * (`shared/api/unwrap.ts`, R-A6) instead of being re-derived here — see
     * issue #132 for why per-call-site unwrapping kept producing this bug.
     */
    const rows = adaptCurrentMessageRows(unwrapList<unknown>(messageListQuery.data, 'conversation.messageList'));
    return {
      id: detailsQuery.data.id,
      ...(detailsQuery.data.uuid !== undefined ? { uuid: detailsQuery.data.uuid } : {}),
      name: detailsQuery.data.name,
      participants: detailsQuery.data.participants ? [...detailsQuery.data.participants] : [],
      message_groups: rows,
      ...(detailsQuery.data.meta !== undefined ? { meta: detailsQuery.data.meta } : {}),
    };
  }, [conversationId, detailsQuery.data, messageListQuery.data]);

  const details = conversationId ? detailsQuery.data : undefined;
  const localWork = useMemo(() => (isLocalWorkConversation(details) ? { folderName: localWorkFolderName(details?.meta) } : undefined), [details]);
  return { activeConversation, isLoading: enabled && (detailsQuery.isLoading || messageListQuery.isLoading), localWork };
}

/** @public Composes real project/user/conversation data for the `/chat` page. */
export function useChatPageData({ conversationId }: UseChatPageDataParams): UseChatPageDataResult {
  const { project } = useSelectedProject();
  const authorQuery = useGetCurrentAuthor();
  const author = currentAuthorOf(authorQuery.data);

  const projectId = project?.id ?? author?.personal_project_id;
  const { activeConversation, isLoading, localWork } = useActiveConversation(projectId, conversationId);

  const user = author ? { id: author.id, name: author.name, avatar: author.avatar } : undefined;

  return { projectId, user, activeConversation, isLoadingConversation: isLoading, localWork };
}
