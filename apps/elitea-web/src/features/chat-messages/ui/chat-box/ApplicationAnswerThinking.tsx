/**
 * Split out of `ApplicationAnswer.tsx` to stay under the file-length budget
 * (§3.5) — the tool-call/thinking-step view: coordinator-level actions
 * render flat via `ActionView`, sub-agent-grouped actions render in
 * `SubAgentAccordion`. A minimal stand-in for the baseline's
 * `ApplicationThinkView` (full streaming-liveness parity is out of scope,
 * see `ApplicationAnswer.tsx`'s module doc).
 */
import type { ReactNode, SyntheticEvent } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';

import { ActionView } from '../ActionView';
import type { ActionViewProps } from '../ActionView';
import { SubAgentAccordion } from '../sub-agent-section/SubAgentAccordion';
import type { SubAgentTool } from '../sub-agent-section/subAgentIcon.helpers';

import { convertJsonToString } from '@/shared/lib/json';
import type { ToolActionDraft } from '@/entities/message/lib/toolActions';

import { partitionActionsIntoBlocks } from '../../lib/subAgentGrouping';
import type { PartitionedBlock, SubAgentGroupable } from '../../lib/subAgentGrouping';
import { getActionOwnerPath, getSubAgentInstanceKey, getSubAgentName } from '../../lib/executionHierarchy';
import { computeBreadcrumbs, selectRichestAgentPath } from '../../lib/subAgentBreadcrumbs';
import { subAgentActionOutputText } from '../../lib/subAgentActionOutput';
import type { SubAgentTypeByName } from '../../lib/approvalBuckets';

/** Loosely-typed defensive read of a `SubAgentGroupable`'s richer runtime fields (id/content/status/toolOutputs/...). */
export function asDraft(action: SubAgentGroupable): ToolActionDraft {
  return action as unknown as ToolActionDraft;
}

export function actionKey(action: SubAgentGroupable, index: number): string {
  const draft = asDraft(action);
  return draft.id || `${draft.type}-${index}`;
}

/** Owner of an action: the reference's `getSubAgentName` (`''` = coordinator). */
function deriveActionName(action: SubAgentGroupable): string {
  return getSubAgentName(asDraft(action));
}

/** One key per sub-agent INSTANCE (owner path call ids), never the bare name. */
function deriveActionInstanceKey(action: SubAgentGroupable): string {
  return getSubAgentInstanceKey(asDraft(action));
}

/**
 * The reference's `classifyWrapper` (`ApplicationThinkView.jsx`): the
 * delegation wrapper erroring without deferring marks its instance paused.
 */
function classifyWrapper(action: SubAgentGroupable, name: string): 'paused' | 'active' | null {
  const draft = asDraft(action);
  const isInner = Boolean(draft.parent_agent_name || draft.toolMeta?.['parent_agent_name']);
  const isWrapper = draft.type === 'tool' && !isInner && (draft.name === name || draft.original_name === name);
  if (!isWrapper) return null;
  const deferred = Boolean(draft['hitlDeferred'] || draft.toolMeta?.['hitl_deferred']);
  return draft.status === 'error' && !deferred ? 'paused' : 'active';
}

const TERMINAL_STATUSES = new Set(['complete', 'error', 'cancelled']);

function toActionViewAction(action: SubAgentGroupable, inSubAgent = false): ActionViewProps['action'] {
  const draft = asDraft(action);
  // Inside a sub-agent group a saved child's `{response}` envelope shows as
  // the child's own result (`subAgentActionOutputText`).
  const toolOutputs = draft.toolOutputs === undefined
    ? undefined
    : inSubAgent ? subAgentActionOutputText(draft.toolOutputs) : convertJsonToString(draft.toolOutputs);
  return {
    type: draft.type,
    status: draft.status,
    timestamp: draft.timestamp,
    ...(draft.name !== undefined ? { name: draft.name } : {}),
    ...(draft.content !== undefined ? { content: draft.content } : {}),
    ...(draft.toolInputs !== undefined ? { toolInputs: draft.toolInputs } : {}),
    ...(toolOutputs !== undefined ? { toolOutputs } : {}),
    ...(draft.toolMeta !== undefined ? { toolMeta: draft.toolMeta } : {}),
    ...(draft.isError !== undefined ? { isError: draft.isError } : {}),
    // A restored pin's row identity: this mapper is an explicit WHITELIST, so
    // a field left out here is a field the modal can never see (#951).
    ...(draft.traceStepId !== undefined ? { traceStepId: draft.traceStepId } : {}),
    ...(draft.traceMessageGroupId !== undefined ? { traceMessageGroupId: draft.traceMessageGroupId } : {}),
  };
}

export function swarmChildContent(action: SubAgentGroupable): string {
  const draft = asDraft(action);
  if (typeof draft.content === 'string' && draft.content) return draft.content;
  if (typeof draft.toolOutputs === 'string') return draft.toolOutputs;
  return draft.toolOutputs !== undefined ? convertJsonToString(draft.toolOutputs) : '';
}

/**
 * `calculateDuration` (`apps/elitea-ui/src/[fsd]/features/chat/lib/helpers/
 * chat.helpers.js:26-46`), ported verbatim including its "1 sec" / "secs" /
 * "less than a second" wording.
 */
function calculateDuration(startMs: number, endMs: number): string {
  const durationMs = endMs - startMs;
  const seconds = Math.floor((durationMs / 1000) % 60);
  const minutes = Math.floor((durationMs / (1000 * 60)) % 60);
  const hours = Math.floor(durationMs / (1000 * 60 * 60));
  if (hours) return t('features.chatMessages.durationHours', '{{hours}} h {{minutes}} min and {{seconds}} sec', { hours, minutes, seconds });
  if (minutes) return t('features.chatMessages.durationMinutes', '{{minutes}} min and {{seconds}} sec', { minutes, seconds });
  if (seconds > 1) return t('features.chatMessages.durationSeconds', '{{seconds}} secs', { seconds });
  if (seconds > 0) return t('features.chatMessages.durationOneSecond', '1 sec');
  return t('features.chatMessages.durationSubSecond', 'less than a second');
}

/**
 * Wall-clock span of the whole turn: earliest start → latest end across ALL
 * actions, not the positional first/last (baseline `ApplicationThinkView.jsx`'s
 * own `thoughtDuration` memo and its #4993 note about interleaved fan-out).
 */
function thoughtDuration(actions: readonly SubAgentGroupable[]): string {
  let minStart = Infinity;
  let maxEnd = -Infinity;
  for (const action of actions) {
    const draft = asDraft(action);
    const start = new Date(draft.created_at ?? draft.timestamp).getTime();
    if (!Number.isNaN(start)) minStart = Math.min(minStart, start);
    const end = new Date(draft.timestamp ?? draft.ended_at ?? draft.created_at).getTime();
    if (!Number.isNaN(end)) maxEnd = Math.max(maxEnd, end);
  }
  if (minStart === Infinity || maxEnd === -Infinity) return calculateDuration(0, 0);
  return calculateDuration(minStart, maxEnd);
}

/**
 * The accordion shell, measured off the live production row: a single
 * `border.table` hairline UNDER the summary, 8px of padding below it, and a
 * pill-shaped 24px-tall summary whose chevron leads on the left with the
 * label 8px after it. Nothing is shown expanded at rest — the reference
 * transcript shows one collapsed `> Thought for 1 sec` line, where this
 * component used to dump every raw reasoning step inline under a
 * `Thinking step` heading.
 */
const thinkingSlotSx = {
  accordion: (theme: Theme) => ({
    background: 'transparent',
    width: '100%',
    borderBottom: `0.0625rem solid ${theme.vars.palette.border.table}`,
    paddingBottom: '0.5rem',
    '&.Mui-expanded': { margin: 0 },
    '& .MuiAccordion-heading': { display: 'inline-block' },
  }),
  summary: {
    width: 'auto',
    minHeight: '1.5rem',
    borderRadius: '1rem',
    padding: '0 0.5rem',
  },
  details: {
    paddingTop: '0.75rem',
    paddingBottom: '1rem',
    paddingLeft: '2rem',
    paddingRight: '0.75rem',
    display: 'flex',
    flexDirection: 'column',
    gap: '1rem',
    width: '100%',
    boxSizing: 'border-box',
  },
} as const;

/** @public Props for `ApplicationAnswerThinking`. */
export interface ApplicationAnswerThinkingProps {
  readonly actions: readonly SubAgentGroupable[];
  /** True only while work is really running; a parked (all-paused) turn is not streaming. */
  readonly isStreaming?: boolean;
  /** The participant's tools — header icons (reference `resolveSubAgentIcon`). */
  readonly tools?: readonly SubAgentTool[] | undefined;
  readonly subAgentTypeByName?: SubAgentTypeByName | undefined;
  /** Instance keys (`getSubAgentInstanceKey` shape) that own a still-pending card. */
  readonly pendingInstanceKeys?: ReadonlySet<string> | undefined;
}

type SubBlock = Extract<PartitionedBlock, { kind: 'sub' }>;

/** One labelled instance of the thinking tree. */
interface DecoratedSub {
  readonly block: SubBlock;
  readonly label: string;
  readonly agentType: string;
}

function blockAgentType(block: SubBlock, byName: SubAgentTypeByName | undefined): string {
  const participant = byName?.[block.name];
  if (participant) return participant === 'pipeline' ? 'pipeline' : 'application';
  let best = '';
  for (const action of block.actions) {
    const draft = asDraft(action);
    const type = draft.toolMeta?.['toolkit_type'] ?? draft.toolMeta?.['agent_type'];
    if (type === 'pipeline') return 'pipeline';
    if (type === 'application' || (typeof draft.toolMeta?.['agent_type'] === 'string' && draft.toolMeta['agent_type'])) best = 'application';
  }
  return best;
}

/**
 * Labels per instance (reference `partitionIntoBlocks`): the breadcrumb of
 * the richest owner path, else the legacy `${name} (${ordinal})` for
 * same-name blocks without any path.
 */
function decorate(blocks: readonly PartitionedBlock[], byName: SubAgentTypeByName | undefined): Map<string, DecoratedSub> {
  const subs = blocks.filter((block): block is SubBlock => block.kind === 'sub');
  const totals = new Map<string, number>();
  for (const block of subs) totals.set(block.name, (totals.get(block.name) ?? 0) + 1);
  const paths = new Map(subs.map((block) => [block.instanceKey, selectRichestAgentPath(block.actions.map((action) => getActionOwnerPath(asDraft(action))))]));
  const crumbs = computeBreadcrumbs(subs.map((block) => ({ instanceKey: block.instanceKey, agentPath: paths.get(block.instanceKey) ?? [] })));
  const seen = new Map<string, number>();
  const result = new Map<string, DecoratedSub>();
  for (const block of subs) {
    const seq = (seen.get(block.name) ?? 0) + 1;
    seen.set(block.name, seq);
    const fallback = (totals.get(block.name) ?? 0) > 1 ? `${block.name} (${seq})` : block.name;
    result.set(block.instanceKey, { block, label: crumbs.get(block.instanceKey) ?? fallback, agentType: blockAgentType(block, byName) });
  }
  return result;
}

/** Running while the instance still has non-terminal work and its wrapper has not returned. */
function blockRunning(block: SubBlock): boolean {
  return block.actions.some((action) => !TERMINAL_STATUSES.has(asDraft(action).status));
}

function ThinkBlocks({ blocks, decorated, isStreaming, tools, pendingInstanceKeys }: {
  readonly blocks: readonly PartitionedBlock[];
  readonly decorated: ReadonlyMap<string, DecoratedSub>;
  readonly isStreaming: boolean;
  readonly tools: readonly SubAgentTool[] | undefined;
  readonly pendingInstanceKeys: ReadonlySet<string> | undefined;
}): ReactNode {
  // Coordinator runs and sub-agent instances interleave in timeline order
  // (reference `StreamingThinkBlocks`), never coordinator-first.
  return blocks.map((block, index) => {
    if (block.kind === 'coord') {
      return block.actions.map((action, actionIndex) => (
        <ActionView key={`coord-${index}-${actionKey(action, actionIndex)}`} action={toActionViewAction(action)} />
      ));
    }
    const entry = decorated.get(block.instanceKey);
    const paused = block.pausedForResume || Boolean(pendingInstanceKeys?.has(block.instanceKey));
    return (
      <SubAgentAccordion
        key={`sa-${block.instanceKey}`}
        name={block.name}
        label={entry?.label}
        tools={tools}
        agentType={entry?.agentType}
        running={isStreaming && blockRunning(block)}
        paused={paused}
      >
        {block.actions.map((action, actionIndex) => (
          <ActionView key={`${block.instanceKey}-${actionKey(action, actionIndex)}`} action={toActionViewAction(action, true)} />
        ))}
      </SubAgentAccordion>
    );
  });
}

export function ApplicationAnswerThinking({
  actions,
  isStreaming = false,
  tools,
  subAgentTypeByName,
  pendingInstanceKeys,
}: ApplicationAnswerThinkingProps): ReactNode {
  /*
   * The panel's own open state is always defined, so the accordion stays
   * controlled for its whole life (MUI fixes controlled-vs-uncontrolled on
   * first render). Reference rule (`ApplicationThinkView.jsx`):
   * `expanded={isStreaming || expanded}`, `onChange` disabled while truly
   * streaming, and a live tree stays expanded once it settles or parks —
   * the reader may then collapse it.
   */
  const [openedByReader, setOpenedByReader] = useState(false);
  useEffect(() => {
    if (isStreaming) setOpenedByReader(true);
  }, [isStreaming]);
  const handleToggle = useCallback((_event: SyntheticEvent, value: boolean) => {
    setOpenedByReader(value);
  }, []);

  const blocks = useMemo(
    () => partitionActionsIntoBlocks(actions, { deriveName: deriveActionName, deriveInstanceKey: deriveActionInstanceKey, classifyWrapper }),
    [actions],
  );
  const decorated = useMemo(() => decorate(blocks, subAgentTypeByName), [blocks, subAgentTypeByName]);

  if (!actions.length) return null;

  return (
    <BasicAccordion
      data-testid="chat-answer-thought-accordion"
      uppercase={false}
      showMode="left"
      defaultExpanded={false}
      expanded={isStreaming || openedByReader}
      {...(isStreaming ? {} : { onChange: handleToggle })}
      slotSx={{ root: { width: '100%' }, ...thinkingSlotSx }}
      items={[
        {
          title: t('features.chatMessages.thoughtFor', 'Thought for {{duration}}', { duration: thoughtDuration(actions) }),
          content: (
            <Box sx={{ width: '100%' }}>
              <ThinkBlocks blocks={blocks} decorated={decorated} isStreaming={isStreaming} tools={tools} pendingInstanceKeys={pendingInstanceKeys} />
            </Box>
          ),
        },
      ]}
    />
  );
}
