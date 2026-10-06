import { useQuery } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { queryKeys } from '@/api/queries/keys';

/**
 * Like / dislike on an answer, with an optional comment (client contract
 * 1.3: getMessageFeedback, setMessageFeedback, deleteMessageFeedback). The
 * message id is the message GROUP uuid (ChatMessage.uuid). A second rating
 * from the same user replaces the first; DELETE retracts it. Every call
 * answers the updated summary. Online only, never cached offline.
 */

/** The server refuses longer comments with 400 (messageFeedbackCommentMaxLen). */
export const MAX_FEEDBACK_COMMENT = 2000;

export type Rating = 1 | -1;

export interface FeedbackSummary {
  likes: number;
  dislikes: number;
  /** The caller's own rating; null when they have not rated this answer. */
  mine: { rating: Rating; comment: string | null } | null;
}

const Count = z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER);
const Summary = z.looseObject({
  likes: Count,
  dislikes: Count,
  mine: z
    .looseObject({ rating: z.union([z.literal(1), z.literal(-1)]), comment: z.string().max(10_000).nullish() })
    .nullish(),
});

export type FeedbackFailure = 'not_allowed' | 'not_available' | 'invalid' | 'failed';

export class FeedbackError extends Error {
  constructor(readonly reason: FeedbackFailure, readonly status?: number) {
    super(`feedback: ${reason}`);
    this.name = 'FeedbackError';
  }
}

export function feedbackErrorText(error: unknown): string {
  if (!(error instanceof FeedbackError)) return "Your feedback wasn't sent. Try again.";
  switch (error.reason) {
    case 'not_allowed':
      return "You can't rate answers in this project.";
    case 'not_available':
      return 'This workspace does not take feedback from mobile yet.';
    case 'invalid':
      return 'The workspace refused this feedback. Shorten the comment and try again.';
    default:
      return "Your feedback wasn't sent. Try again.";
  }
}

function failureOf(status: number): FeedbackError {
  if (status === 403) return new FeedbackError('not_allowed', status);
  if (status === 404 || status === 501) return new FeedbackError('not_available', status);
  if (status === 400 || status === 422) return new FeedbackError('invalid', status);
  return new FeedbackError('failed', status);
}

function summaryOf(data: unknown): FeedbackSummary {
  const parsed = Summary.safeParse(data);
  if (!parsed.success) throw new FeedbackError('failed');
  const mine = parsed.data.mine ?? null;
  return {
    likes: parsed.data.likes,
    dislikes: parsed.data.dislikes,
    mine: mine ? { rating: mine.rating, comment: mine.comment ?? null } : null,
  };
}

const MESSAGE_ID = /^[0-9a-fA-F-]{36}$/;
const PATH = '/elitea_core/message_feedback/prompt_lib/{project_id}/{message_id}' as const;

function params(projectId: number, messageUuid: string) {
  if (!Number.isSafeInteger(projectId) || projectId <= 0 || !MESSAGE_ID.test(messageUuid)) {
    throw new FeedbackError('invalid');
  }
  return { path: { project_id: String(projectId), message_id: messageUuid } };
}

export async function fetchFeedback(projectId: number, messageUuid: string, client: ApiClient = sessionApiClient()): Promise<FeedbackSummary> {
  const { data, response } = await client.GET(PATH, { params: params(projectId, messageUuid) });
  if (!response.ok) throw failureOf(response.status);
  return summaryOf(data);
}

/** Records (or replaces) the caller's rating. An empty comment is not sent. */
export async function setFeedback(
  projectId: number,
  messageUuid: string,
  rating: Rating,
  comment: string | null = null,
  client: ApiClient = sessionApiClient(),
): Promise<FeedbackSummary> {
  const trimmed = comment?.trim() ?? '';
  if (trimmed.length > MAX_FEEDBACK_COMMENT) throw new FeedbackError('invalid');
  const { data, response } = await client.POST(PATH, {
    params: params(projectId, messageUuid),
    body: trimmed === '' ? { rating } : { rating, comment: trimmed },
  });
  if (!response.ok) throw failureOf(response.status);
  return summaryOf(data);
}

/** Retracts the caller's own rating (others' are untouched). */
export async function clearFeedback(projectId: number, messageUuid: string, client: ApiClient = sessionApiClient()): Promise<FeedbackSummary> {
  const { data, response } = await client.DELETE(PATH, { params: params(projectId, messageUuid) });
  if (!response.ok) throw failureOf(response.status);
  return summaryOf(data);
}

/** The caller's rating of one answer, read when the actions sheet opens it. */
export function useFeedback(projectId: number | null, messageUuid: string | null, enabled: boolean) {
  return useQuery({
    queryKey: [...queryKeys.feedback(projectId ?? 0), messageUuid ?? ''],
    queryFn: () => fetchFeedback(projectId!, messageUuid!),
    enabled: enabled && projectId !== null && messageUuid !== null,
    staleTime: 30_000,
    retry: false,
  });
}
