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

import { likeApplication, likeEntity, unlikeApplication, unlikeEntity } from '@/shared/api/generated/social/social';
import { EliteaApiError } from '@/shared/api/generated/mutator';

/* ── Store ──────────────────────────────────────────────────────────── */

/**
 * One like the user toggled, with the server's own values it was toggled FROM.
 * A later read that carries different server values (another person liked it,
 * or the list was refetched after this like) wins over the local entry: the
 * entry only stands while the server still says what it said then.
 */
interface LikeEntry {
  readonly liked: boolean;
  readonly count: number;
  readonly baseLiked: boolean;
  readonly baseCount: number;
}

interface LikeState {
  entries: Record<string, LikeEntry>;
  setEntry: (id: string, entry: LikeEntry) => void;
  clearEntry: (id: string) => void;
}

const useLikeStore = create<LikeState>(set => ({
  entries: {},
  setEntry: (id, entry) => set(state => ({ entries: { ...state.entries, [id]: entry } })),
  clearEntry: id =>
    set(state => ({ entries: Object.fromEntries(Object.entries(state.entries).filter(([key]) => key !== id)) })),
}));

/* ── Hook ───────────────────────────────────────────────────────────── */

/**
 * What a like names. An agent is liked as the `application` entity, a skill as
 * the `skill` entity (skills epic 2169, S6: the generic like route takes any
 * entity name). The two share one id space per table, so the store keys a like
 * by both.
 */
export type CardLikeEntityType = 'application' | 'skill';

export interface UseCardLikeOptions {
  /** The entity's id (the public twin's id for a Marketplace listing). Named for its first use, the agents hub. */
  applicationId: string;
  /** Default `application`. */
  entityType?: CardLikeEntityType;
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
  /** Called when the server refused the like or unlike and the optimistic change was reverted, so the caller can say so. */
  onError?: () => void;
}

export function useCardLike({
  applicationId,
  entityType = 'application',
  projectId,
  initialLiked = false,
  initialCount = 0,
  onLikeSuccess,
  onError,
}: UseCardLikeOptions) {
  const entry = useLikeStore(state => state.entries[`${entityType}:${applicationId}`]);
  const setEntry = useLikeStore(state => state.setEntry);
  const clearEntry = useLikeStore(state => state.clearEntry);

  // An agent and a skill can share a numeric id: key the store by both.
  const storeKey = `${entityType}:${applicationId}`;
  // The local entry stands only while the server still says what it said when
  // the entry was made; a refetch with other values wins.
  const current = entry !== undefined && entry.baseLiked === initialLiked && entry.baseCount === initialCount ? entry : undefined;
  const isLiked = current?.liked ?? initialLiked;
  const likeCount = current?.count ?? initialCount;
  const [isToggling, setIsToggling] = useState(false);
  // A ref as well as the state: two clicks in one tick both read the state as false.
  const inFlight = useRef(false);

  const toggleLike = useCallback(async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    setIsToggling(true);

    const newLiked = !isLiked;
    const newCount = newLiked ? likeCount + 1 : Math.max(0, likeCount - 1);

    // Optimistic update — reverted below if the server call fails.
    setEntry(storeKey, { liked: newLiked, count: newCount, baseLiked: initialLiked, baseCount: initialCount });

    const numericApplicationId = Number(applicationId);
    try {
      if (!Number.isFinite(numericApplicationId)) {
        throw new TypeError(
          `useCardLike: applicationId "${applicationId}" is not numeric — /social/like/prompt_lib requires a numeric path segment`,
        );
      }
      if (entityType === 'skill') {
        await (newLiked ? likeEntity : unlikeEntity)(projectId, 'skill', numericApplicationId);
      } else if (newLiked) {
        await likeApplication(projectId, numericApplicationId);
      } else {
        await unlikeApplication(projectId, numericApplicationId);
      }
      onLikeSuccess?.(applicationId, newLiked, newCount);
    } catch (error) {
      // Revert the optimistic update — the like did not actually persist
      // (see this file's module doc comment for the known backend defect).
      clearEntry(storeKey);
      // Only the documented API-failure path is swallowed; anything else
      // (a programmer error, e.g. the NaN guard above) still surfaces.
      if (!(error instanceof EliteaApiError)) throw error;
      onError?.();
    } finally {
      inFlight.current = false;
      setIsToggling(false);
    }
  }, [isLiked, likeCount, applicationId, entityType, storeKey, projectId, initialLiked, initialCount, setEntry, clearEntry, onLikeSuccess, onError]);

  return { isLiked, likeCount, isToggling, toggleLike };
}
