/**
 * Split out of `ApplicationAnswer.tsx` to stay under the file-length budget
 * (§3.5) — the tool-call/thinking-step view: coordinator-level actions
 * render flat via `ActionView`, sub-agent-grouped actions render in
 * `SubAgentAccordion`. A minimal stand-in for the baseline's
 * `ApplicationThinkView` (full streaming-liveness parity is out of scope,
 * see `ApplicationAnswer.tsx`'s module doc).
 */
import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';

import { ActionView } from '../ActionView';
import type { ActionViewProps } from '../ActionView';
import { SubAgentAccordion } from '../sub-agent-section/SubAgentAccordion';

import { convertJsonToString } from '@/shared/lib/json';
import type { ToolActionDraft } from '@/entities/message/lib/toolActions';

import { partitionActionsIntoBlocks } from '../../lib/subAgentGrouping';
import type { SubAgentGroupable } from '../../lib/subAgentGrouping';

/** Loosely-typed defensive read of a `SubAgentGroupable`'s richer runtime fields (id/content/status/toolOutputs/...). */
export function asDraft(action: SubAgentGroupable): ToolActionDraft {
  return action as unknown as ToolActionDraft;
}

export function actionKey(action: SubAgentGroupable, index: number): string {
  const draft = asDraft(action);
  return draft.id || `${draft.type}-${index}`;
}

function deriveActionName(action: SubAgentGroupable): string {
  const draft = asDraft(action);
  return draft.parent_agent_name || draft.original_name || '';
}

function deriveActionInstanceKey(action: SubAgentGroupable): string {
  const draft = asDraft(action);
  return draft.parent_agent_call_id || deriveActionName(action);
}

function toActionViewAction(action: SubAgentGroupable): ActionViewProps['action'] {
  const draft = asDraft(action);
  const toolOutputs = draft.toolOutputs === undefined ? undefined : convertJsonToString(draft.toolOutputs);
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
  /** Keeps the panel open while the turn is still running (baseline: `expanded={isStreaming || expanded}`). */
  readonly isStreaming?: boolean;
}

export function ApplicationAnswerThinking({ actions, isStreaming = false }: ApplicationAnswerThinkingProps): ReactNode {
  const blocks = useMemo(
    () =>
      partitionActionsIntoBlocks(actions, {
        deriveName: deriveActionName,
        deriveInstanceKey: deriveActionInstanceKey,
        classifyWrapper: () => null,
      }),
    [actions],
  );
  const coordActions = useMemo(() => blocks.flatMap((block) => (block.kind === 'coord' ? block.actions : [])), [blocks]);

  if (!actions.length) return null;

  return (
    <BasicAccordion
      data-testid="chat-answer-thought-accordion"
      uppercase={false}
      showMode="left"
      defaultExpanded={false}
      {...(isStreaming ? { expanded: true } : {})}
      slotSx={{ root: { width: '100%' }, ...thinkingSlotSx }}
      items={[
        {
          title: t('features.chatMessages.thoughtFor', 'Thought for {{duration}}', { duration: thoughtDuration(actions) }),
          content: (
            <Box sx={{ width: '100%' }}>
              {coordActions.map((action, index) => (
                <ActionView key={actionKey(action, index)} action={toActionViewAction(action)} />
              ))}
              <SubAgentAccordion blocks={blocks} />
            </Box>
          ),
        },
      ]}
    />
  );
}
