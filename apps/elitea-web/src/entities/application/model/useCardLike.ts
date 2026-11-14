/**
 * useCardLike — zustand-backed hook for managing card-like state.
 *
 * Replaces the old Redux-based `Like` component with a lightweight
 * zustand store. Used by agents-hub `AgentHubLike` and any future
 * card-surface like button.
 *
 * @public Wave-2 surface: consumed by pages/agents-hub/ui/AgentHubLike.
 *
 * ── Adversarial-review fix (cluster A13-agents-hub, finding 1) ──────────
 * Previously `toggleLike` only flipped the local zustand flags — it never
 * called the server, so a like never persisted and reloading (or another
 * device/session) never reflected it. `toggleLike` now calls the REAL
 * generated `likeApplication`/`unlikeApplication` functions
 * (`shared/api/generated/social/social.ts`,
 * `POST`/`DELETE /social/like/prompt_lib/{projectId}/application/{applicationId}`,
 * `internal/api/v2/social/handler.go:239-304`) and reverts the optimistic
 * local update if the call fails, so the UI never claims a like persisted
 * when it didn't.
 *
 * The plain async functions are called directly (not the generated
 * `useLikeApplication`/`useUnlikeApplication` hooks): despite the
 * "Like"/"Unlike" naming those are orval-generated as `useQuery`-shaped
 * hooks (auto-fetch-on-mount), not mutations — wrong shape for a
 * click-triggered action. Calling the underlying `likeApplication`/
 * `unlikeApplication` async functions imperatively inside the click
 * handler is the correct fit, and is still the exact same
 * manifest-registered endpoint/transport (`eliteaFetch`) the hooks use.
 *
 * **Known, disclosed, NOT-fixable-from-here backend defect:** the
 * generated client's own doc comment on `likeApplication`
 * (`social.ts:930-945`) records that `centry.social_likes`'s real migrated
 * shape does not match the columns the Go handler inserts into, so this
 * endpoint is currently expected to 500 on every real call. That is a
 * `services/elitea-main` defect, entirely outside this cluster's file
 * scope (routing/persistence, not `apps/elitea-web`). This file's job is
 * only to call the real, correct endpoint and handle failure honestly
 * (revert the optimistic UI) — it cannot make the backend table shape
 * correct.
 */
import { useCallback, useRef, useState } from 'react';

import { create } from 'zustand';

import { likeApplication, unlikeApplication } from '@/shared/api/generated/social/social';
import { EliteaApiError } from '@/shared/api/generated/mutator';

/* ── Store ──────────────────────────────────────────────────────────── */

interface LikeState {
  likedStates: Record<string, boolean>;
  likeCounts: Record<string, number>;
  setLikeState: (id: string, liked: boolean) => void;
  setLikeCount: (id: string, count: number) => void;
}

const useLikeStore = create<LikeState>(set => ({
  likedStates: {},
  likeCounts: {},
  setLikeState: (id, liked) =>
    set(state => ({
      likedStates: { ...state.likedStates, [id]: liked },
    })),
  setLikeCount: (id, count) =>
    set(state => ({
      likeCounts: { ...state.likeCounts, [id]: count },
    })),
}));

/* ── Hook ───────────────────────────────────────────────────────────── */

export interface UseCardLikeOptions {
  applicationId: string;
  /**
   * Owning project id — for every public agents-hub card this is always the
   * public project (`ApplicationData.project_id`, "Always the public
   * project id" per `PublicApplicationSummary`'s own NOTE(W2)), passed by
   * the caller rather than looked up here because the like endpoint is
   * project-scoped (`POST /social/like/prompt_lib/{projectId}/application/
   * {applicationId}`) and this entity hook has no business knowing which
   * project a given card belongs to.
   */
  projectId: string;
  initialLiked?: boolean;
  initialCount?: number;
  onLikeSuccess?: (applicationId: string, isLiked: boolean, likeCount: number) => void;
}

export function useCardLike({
  applicationId,
  projectId,
  initialLiked = false,
  initialCount = 0,
  onLikeSuccess,
}: UseCardLikeOptions) {
  const likedStates = useLikeStore(state => state.likedStates);
  const likeCounts = useLikeStore(state => state.likeCounts);
  const setLikeState = useLikeStore(state => state.setLikeState);
  const setLikeCount = useLikeStore(state => state.setLikeCount);

  const isLiked = likedStates[applicationId] ?? initialLiked;
  const likeCount = likeCounts[applicationId] ?? initialCount;
  const [isToggling, setIsToggling] = useState(false);
  // A ref as well as the state: two clicks in one tick both read the state as false.
  const inFlight = useRef(false);

  const toggleLike = useCallback(async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    setIsToggling(true);

    const wasLiked = isLiked;
    const newLiked = !wasLiked;
    const previousCount = likeCount;
    const newCount = newLiked ? previousCount + 1 : Math.max(0, previousCount - 1);

    // Optimistic update — reverted below if the server call fails.
    setLikeState(applicationId, newLiked);
    setLikeCount(applicationId, newCount);

    const numericApplicationId = Number(applicationId);
    try {
      if (!Number.isFinite(numericApplicationId)) {
        throw new TypeError(
          `useCardLike: applicationId "${applicationId}" is not numeric — /social/like/prompt_lib requires a numeric path segment`,
        );
      }
      if (newLiked) {
        await likeApplication(projectId, numericApplicationId);
      } else {
        await unlikeApplication(projectId, numericApplicationId);
      }
      onLikeSuccess?.(applicationId, newLiked, newCount);
    } catch (error) {
      // Revert the optimistic update — the like did not actually persist
      // (see this file's module doc comment for the known backend defect).
      setLikeState(applicationId, wasLiked);
      setLikeCount(applicationId, previousCount);
      // Only the documented API-failure path is swallowed; anything else
      // (a programmer error, e.g. the NaN guard above) still surfaces.
      if (!(error instanceof EliteaApiError)) throw error;
    } finally {
      inFlight.current = false;
      setIsToggling(false);
    }
  }, [isLiked, likeCount, applicationId, projectId, setLikeState, setLikeCount, onLikeSuccess]);

  return { isLiked, likeCount, isToggling, toggleLike };
}
