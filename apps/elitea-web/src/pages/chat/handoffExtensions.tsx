/**
 * What the chat page puts in the composer's place (or above it) for a thread
 * that was handed between a desktop and the cloud (ADR-0034), kept out of
 * `index.tsx` for its complexity ceiling:
 *
 * - `local`, and the thread is Local work: the read-only notice, as before;
 * - `cloud`: the composer works, with the Cloud executor notice above it;
 * - `returning`: the notice replaces the composer;
 * - the newest answer ran on a desktop: no regenerate (the server refuses it).
 */
import { lazy, Suspense } from 'react';

import type { ChatBoxProps } from '@/widgets/chat-box';

import { LocalWorkNotice } from './LocalWorkNotice';
import type { UseChatPageDataResult } from './useChatPageData';

// Loaded when a thread is in the cloud: nothing else needs it.
const CloudExecutorNotice = lazy(() => import('./CloudExecutorNotice'));

type Extensions = NonNullable<ChatBoxProps['extensions']>;

export interface HandoffExtensionsInput {
  readonly localWork: UseChatPageDataResult['localWork'];
  readonly handoff: UseChatPageDataResult['handoff'];
  readonly projectId: string | undefined;
  readonly conversationId: string | undefined;
  readonly userId: string | undefined;
  /** The composer's existing context indicator, which stays under the cloud notice. */
  readonly contextIndicator: React.ReactNode;
}

export function handoffExtensions({ localWork, handoff, projectId, conversationId, userId, contextIndicator }: HandoffExtensionsInput): Pick<Extensions, 'readOnlyNotice' | 'contextIndicator' | 'noRegenerate'> {
  const noRegenerate = handoff.lastAnswerOnDesktop ? { noRegenerate: true } : {};
  if (conversationId === undefined) return { contextIndicator };
  if (handoff.executor !== 'local' && projectId !== undefined && handoff.uuid !== undefined) {
    const notice = (
      <Suspense fallback={null}>
        <CloudExecutorNotice
          projectId={projectId}
          conversationUuid={handoff.uuid}
          executor={handoff.executor}
          isOwner={userId !== undefined && handoff.authorId === userId}
        />
      </Suspense>
    );
    if (handoff.executor === 'returning') return { readOnlyNotice: notice, ...noRegenerate };
    return { contextIndicator: <>{notice}{contextIndicator}</>, ...noRegenerate };
  }
  if (localWork !== undefined) return { readOnlyNotice: <LocalWorkNotice conversationId={conversationId} folderName={localWork.folderName} /> };
  return { contextIndicator, ...noRegenerate };
}
