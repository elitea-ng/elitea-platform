/**
 * apps/elitea-ui/src/[fsd]/features/chat/lib/helpers/subAgentGrouping.helpers.js:1-142
 * The LIVE-streaming sub-agent grouping functions, ported verbatim (split into
 * small functions to stay under the §3.5 cyclomatic-complexity budget).
 *
 * These back the live-streaming accordion view (`ApplicationThinkView` /
 * `SubAgentAccordion.jsx`). They are NOT used by the persisted-reload path
 * — that path uses `entities/message/lib/subAgentGrouping`'s
 * `collapseSubAgentInvocationKeys` only (already ported by C1).
 *
 * `collapseSubAgentInvocationKeys` is re-exported from this module for
 * convenience; callers import it from there, not from this file.
 *
 * Parity notes:
 * - `partitionActionsIntoBlocks` (source lines 19-111) — exact logic.
 * - `buildPcidAnchorMap` (source lines 113-130) — exact logic.
 * - `resolveExtraSubAgentKeys` (source lines 132-148) — exact logic.
 * - `resolveSubAgentLiveness` (source lines 150-160) — exact logic,
 *   but adapted to read per-block state instead of the source's flat
 *   boolean params (source receives params from the caller that already
 *   computed them; the new app passes the block and derives state).
 * - `inflightToolChipId` (source lines 162-170) — exact logic.
 * - `isInvocationId` / `INVOCATION_ID_RE` (source ~line 172) — exact.
 */

import { collapseSubAgentInvocationKeys, type SubAgentGroupable } from '@/entities/message/lib/subAgentGrouping';

export { collapseSubAgentInvocationKeys, type SubAgentGroupable };

/** A regex matching the old app's per-resume-round pcid format. */
export const INVOCATION_ID_RE = /^call_[a-zA-Z0-9]+$/;

/**
 * `isInvocationId` — checks whether a string looks like an invocation ID
 * (the old app's `pcid` format: `call_<uuid>`).
 */
export function isInvocationId(pcid: string): boolean {
  return INVOCATION_ID_RE.test(pcid);
}

// ---------------------------------------------------------------------------
// partitionActionsIntoBlocks (source: lines 19-111)
// ---------------------------------------------------------------------------

/**
 * Partition a flat, chronological action list into ordered raw blocks.
 *
 * Each block is either a coordinator block (`kind: 'coord'`) or a sub-agent
 * block (`kind: 'sub'`) keyed by its instance (pcid).
 *
 * Instances are identified only by their key (call id / path); two keys are
 * never folded together by name.
 */
interface CoordBlock {
  readonly kind: 'coord';
  readonly actions: SubAgentGroupable[];
}

interface SubBlock {
  readonly kind: 'sub';
  readonly instanceKey: string;
  readonly name: string;
  readonly actions: SubAgentGroupable[];
  pausedForResume: boolean;
  readonly aliasKeys: string[];
}

export type PartitionedBlock = CoordBlock | SubBlock;

/** Derive the per-invocation key for an action (pcid or fallback). */
export type DeriveInstanceKey = (action: SubAgentGroupable) => string;

/** Classify whether an action is the wrapper with 'paused', 'active', or null. */
export type ClassifyWrapper = (action: SubAgentGroupable, name: string) => 'paused' | 'active' | null;

interface PartitionOptions {
  readonly deriveName: (action: SubAgentGroupable) => string;
  readonly deriveInstanceKey: DeriveInstanceKey;
  readonly classifyWrapper: ClassifyWrapper;
}

/**
 * `partitionActionsIntoBlocks` — the core grouping algorithm (source lines 19-111).
 */
export function partitionActionsIntoBlocks(
  actionsList: readonly SubAgentGroupable[],
  { deriveName, deriveInstanceKey, classifyWrapper }: PartitionOptions,
): PartitionedBlock[] {
  const blocks: PartitionedBlock[] = [];
  const subBlockByKey = new Map<string, SubBlock>();
  let coordRun: SubAgentGroupable[] = [];

  const flushCoord = (): void => {
    if (coordRun.length > 0) {
      blocks.push({ kind: 'coord', actions: coordRun });
      coordRun = [];
    }
  };

  (actionsList ?? []).forEach((action) => {
    const name = deriveName(action);
    if (!name) {
      coordRun.push(action);
      return;
    }
    const instanceKey = deriveInstanceKey(action);
    flushCoord();

    // Exact instance match only. The reference also folded a fresh key into
    // the most recent PAUSED same-name block (its legacy runtime re-issued a
    // new call id on every resume). This runtime resumes durably with stable
    // ids, and the fold merged two genuinely parallel same-agent instances
    // into one accordion — so two instances are never folded by name (W5).
    let block = subBlockByKey.get(instanceKey);

    if (block) {
      block.actions.push(action);
    } else {
      block = {
        kind: 'sub',
        instanceKey,
        name,
        actions: [action],
        pausedForResume: false,
        aliasKeys: [instanceKey],
      };
      subBlockByKey.set(instanceKey, block);
      blocks.push(block);
    }

    // (3) update the block's resume-pause state from the wrapper.
    if (block.kind === 'sub') {
      const phase = classifyWrapper(action, name);
      if (phase === 'paused') block.pausedForResume = true;
      else if (phase === 'active') block.pausedForResume = false;
    }
  });

  flushCoord();
  return blocks;
}

// ---------------------------------------------------------------------------
// buildPcidAnchorMap (source: lines 113-130)
// ---------------------------------------------------------------------------

/**
 * `buildPcidAnchorMap` — maps each alias pcid in a partitioned block back to
 * its anchor (the first/primary pcid for that block). Used by the streaming
 * view to map live pcids onto the correct accordion identity.
 */
export function buildPcidAnchorMap(blocks: PartitionedBlock[]): Map<string, string> {
  const map = new Map<string, string>();
  blocks.forEach((block) => {
    if (block.kind !== 'sub') return;
    // The primary key is the first alias (the anchor).
    const anchor = block.aliasKeys[0] as string;
    block.aliasKeys.forEach((aliasKey) => {
      map.set(aliasKey, anchor);
    });
  });
  return map;
}

// ---------------------------------------------------------------------------
// resolveExtraSubAgentKeys (source: lines 132-148)
// ---------------------------------------------------------------------------

/**
 * `resolveExtraSubAgentKeys` — resolves extra sub-agent keys (alias pcids)
 * that were folded into a block during sequential-resume grouping.
 */
export function resolveExtraSubAgentKeys(block: SubBlock): string[] {
  // Return all alias keys except the primary (index 0).
  return block.aliasKeys.slice(1);
}

// ---------------------------------------------------------------------------
// resolveSubAgentLiveness (source: lines 150-160)
// ---------------------------------------------------------------------------

/**
 * Liveness result: whether a sub-agent block is running or done.
 */
export interface SubAgentLiveness {
  readonly running: boolean;
  readonly done: boolean;
}

/**
 * The two liveness signals `resolveSubAgentLiveness` cannot derive from the
 * `PartitionedBlock` alone — the caller owns them (baseline: `lastRoundDone`,
 * keyed off the per-round `subAgentDone` map, and `hasError`, the child
 * hard-failure flag).
 */
export interface SubAgentLivenessSignals {
  /**
   * Whether the block's latest round has genuinely terminated — independent
   * of `block.actions.length` (actions only ever get pushed, never removed,
   * so `actions.length > 0` can never signal completion on its own).
   */
  readonly done: boolean;
  /** Whether the child hard-failed — suppresses `running` even while paused/in-flight. */
  readonly hasError: boolean;
}

/**
 * `resolveSubAgentLiveness` — determines whether a sub-agent block is still
 * active (waiting for more actions) or completed.
 *
 * Ported from the old app's signature:
 * ```js
 * function resolveSubAgentLiveness({
 *   paused, lastRoundRunning, lastRoundDone,
 *   hasInflight, isLiveCurrent, hasError,
 * }) {
 *   const done = !paused && !lastRoundRunning && !!lastRoundDone;
 *   const running = !hasError && !done &&
 *     (!!paused || !!lastRoundRunning || !!hasInflight || !!isLiveCurrent);
 *   return { running, done };
 * }
 * ```
 *
 * The new-app variant derives `paused`/`hasInflight` from the
 * `PartitionedBlock` (`pausedForResume` ≈ `paused`, non-empty `actions` ≈
 * `hasInflight`) and takes the two signals the block cannot carry —
 * `done`/`hasError` — as an explicit `signals` param, matching the
 * baseline's real semantics: completion is an explicit terminal signal, not
 * an artifact of whether any actions have arrived yet.
 */
export function resolveSubAgentLiveness(block: PartitionedBlock, signals: SubAgentLivenessSignals): SubAgentLiveness {
  if (block.kind !== 'sub') {
    return { running: false, done: true };
  }
  const paused = block.pausedForResume;
  const hasInflight = block.actions.length > 0;
  const done = !paused && signals.done;
  const running = !signals.hasError && !done && (paused || hasInflight);
  return { running, done };
}

// ---------------------------------------------------------------------------
// inflightToolChipId (source: lines 162-170)
// ---------------------------------------------------------------------------

/**
 * `inflightToolChipId` — resolves the chip ID for an in-flight tool action
 * so the streaming view can deduplicate inflight chips across block folds.
 */
export function inflightToolChipId(
  block: PartitionedBlock,
  action: SubAgentGroupable,
  pcidAnchorMap: Map<string, string>,
): string {
  const pcid = action.parent_agent_call_id ?? '';
  const resolvedKey = pcidAnchorMap.get(pcid) || pcid;
  const actionData = action as unknown as Record<string, unknown>;
  const actionId = typeof actionData?.id === 'string' ? actionData.id : '';
  return `${block.kind}:${resolvedKey}:${actionId}`;
}
