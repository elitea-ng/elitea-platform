/**
 * The bottom of a thread: the notice for a thread that is away, the errors,
 * and the composer. Where a message goes depends on where the thread runs:
 *
 * - here: the local agent loop (`session.send`), with Act | Plan and the
 *   agent·version picker;
 * - `cloud`: the SERVER conversation (`useCloudChat`: the chat's own execution
 *   route and stream). The conversation's own agent answers, so the picker is
 *   shown disabled and there is no plan mode;
 * - `returning`: read-only, with its notice.
 *
 * A local thread's footer also shows the context meter (`ContextMeter`, lazy)
 * once a model call reported its occupancy, and the usage chip (`UsageChip`,
 * lazy: the latest turn's tokens, the thread's, a cost only from the
 * catalogue's price, #1227); `/compact` marks the next send.
 * And the model chip (`ModelChip`, `useTurnModel`): the model the next turn
 * runs on, unless the agent pins it or the folder's settings keep it
 * (`/model` opens it, `/model <name>` sets a listed one; an unknown name or
 * a catalogue still loading is said above the composer and sets nothing).
 */
import { lazy, Suspense, useMemo, useState } from 'react';

import { useNavigate } from '@tanstack/react-router';

import { composer, replayView, type WorkspaceTurn } from '@/features/workspace';
import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import { AgentChip, RunLocationChip } from './ComposerControls';
import { HandoffBanner } from './HandoffControls';
import { firstError, SessionNotices } from './sessionParts';
import { useProjectSkills, type ComposerSkill } from './useAgentSkills';
import type { CloudChat } from './useCloudChat';
import { useCustomCommands } from './useCustomCommands';
import type { ThreadHandoff } from './useThreadHandoff';
import type { ThreadSession } from './useThreadSession';
import type { SessionCommands } from './useSessionCommands';
import { useTurnModel, type TurnModelControl } from './useTurnModel';
import { WorkspaceComposer, type WorkspaceComposerProps } from './WorkspaceComposer';

const ContextMeter = lazy(() => import('./ContextMeter'));
const UsageChip = lazy(() => import('./UsageChip'));

export interface ThreadComposerProps {
  ipc: WorkspaceIpc;
  workspaceId: string;
  /** The folder's project: the model chip offers its chat models, and the "/" menu its own skills too. */
  projectId: number;
  conversationId: string;
  session: ThreadSession;
  turn: WorkspaceTurn;
  handoff: ThreadHandoff;
  cloud: CloudChat;
  skills: readonly ComposerSkill[];
}

/** What differs when the thread's messages go to the cloud copy. */
function cloudBinding(cloud: CloudChat): Pick<WorkspaceComposerProps, 'canSend' | 'busy' | 'onSend' | 'onStop' | 'placeholder' | 'showMode'> {
  return {
    canSend: cloud.canSend,
    busy: cloud.streaming,
    onSend: (prompt) => cloud.send(prompt),
    onStop: cloud.stop,
    placeholder: t('workspace.cloud.placeholder', 'Message the cloud copy…'),
    showMode: false,
  };
}

/** The model chip and `/model <name>`: neither for a cloud thread (its own agent answers) nor when no choice is offered. */
function modelBinding(model: TurnModelControl, cloudMode: boolean, chipRef: SessionCommands['modelRef']): Pick<WorkspaceComposerProps, 'modelPicker' | 'onModelCommand'> {
  if (cloudMode || !model.offered) return {};
  return {
    modelPicker: <composer.ModelChip current={model.current} effort={model.effort} models={model.models} onChoose={model.choose} chipRef={chipRef} />,
    onModelCommand: model.command,
  };
}

/** The latest context occupancy of the shown turns: the active one's, else the newest earlier one's. */
function latestMeter(turn: WorkspaceTurn) {
  return turn.view.context ?? turn.earlier.findLast((earlier) => earlier.view.context !== undefined)?.view.context;
}

type Usage = WorkspaceTurn['view']['usage'];

/** Each recorded turn's usage (thread history), replayed once per history, not per render. */
function recordedUsages(history: ThreadSession['history']): { turnId: string; usage: Usage }[] {
  return history.kind === 'local' ? history.turns.map((stored) => ({ turnId: stored.turn_id, usage: replayView(stored.events).usage })) : [];
}

/** The usage of the thread's turns that reported any, oldest first: the recorded ones not shown live, then this visit's. */
function turnUsages(turn: WorkspaceTurn, recorded: readonly { turnId: string; usage: Usage }[]): NonNullable<Usage>[] {
  const live = new Set(turn.earlier.map((earlier) => earlier.turnId));
  if (turn.turnId !== null) live.add(turn.turnId);
  const earlier = recorded.filter((entry) => !live.has(entry.turnId)).map((entry) => entry.usage);
  return [...earlier, ...turn.earlier.map((entry) => entry.view.usage), turn.view.usage].filter((usage): usage is NonNullable<Usage> => usage !== undefined);
}

const NO_USAGE: NonNullable<Usage>[] = [];

/** The footer's context meter and usage chip (both lazy), once a model call reported either. */
function ComposerMeters({ meter, usages, projectId }: { meter: ReturnType<typeof latestMeter>; usages: NonNullable<Usage>[]; projectId: number }): React.JSX.Element | null {
  if (meter === undefined && usages.length === 0) return null;
  return (
    <Suspense fallback={null}>
      {meter === undefined ? null : <ContextMeter meter={meter} />}
      {usages.length === 0 ? null : <UsageChip turns={usages} projectId={projectId} />}
    </Suspense>
  );
}

/**
 * What the "/" menu offers besides the host commands: the custom commands and the project's own skills (after the
 * agent's), both the local host's only (`local`: none for the cloud copy). The project's skills are read once the
 * composer got focus (which also prewarms the send) or `/help` is open (`helpOpen`), which lists them too.
 */
function useSlashSources(ipc: WorkspaceIpc, workspaceId: string, projectId: number, local: boolean, skills: readonly ComposerSkill[], prewarm: () => void, helpOpen: boolean) {
  const customCommands = useCustomCommands(ipc, workspaceId, local);
  const [focused, setFocused] = useState(false);
  const projectSkills = useProjectSkills(projectId, (focused || helpOpen) && local, skills);
  const offered = useMemo(() => [...skills, ...projectSkills], [skills, projectSkills]);
  const onFocus = (): void => {
    setFocused(true);
    prewarm();
  };
  return { customCommands, offered, onFocus };
}

export function ThreadComposer({ ipc, workspaceId, projectId, conversationId, session, turn, handoff, cloud, skills }: ThreadComposerProps): React.JSX.Element {
  const navigate = useNavigate();
  const { commands } = session;
  const cloudMode = handoff.status?.executor === 'cloud';
  const model = useTurnModel(workspaceId, projectId, session.raw, turn, session.history);
  const { customCommands, offered, onFocus } = useSlashSources(ipc, workspaceId, projectId, !cloudMode, skills, session.prewarm, commands.notice === 'help');
  const local: Pick<WorkspaceComposerProps, 'canSend' | 'busy' | 'onSend' | 'onStop'> = {
    canSend: session.canSend && !handoff.offLaptop,
    busy: turn.busy,
    onSend: async (prompt, mentions, picked, command, images) => {
      model.dismissNotice();
      const sent = await session.send(prompt, commands.planMode, mentions, picked, commands.compactNext, model.choice, command, images);
      if (sent && commands.compactNext) commands.compactSent();
      return sent;
    },
    onStop: () => void turn.cancel(),
  };
  const openInChat = conversationId === '' ? undefined : () => void navigate({ to: '/chat/$conversationId', params: { conversationId } });
  const meter = cloudMode ? undefined : latestMeter(turn);
  const recorded = useMemo(() => recordedUsages(session.history), [session.history]);
  const usages = cloudMode ? NO_USAGE : turnUsages(turn, recorded);
  const modelNotice = cloudMode ? null : model.notice;
  const agentHint = cloudMode ? t('workspace.cloud.agentHint', 'The cloud copy answers with this thread’s own agent.') : undefined;
  return (
    <>
      <HandoffBanner handoff={handoff} />
      <SessionNotices error={modelNotice ?? firstError(turn.startError, session.sendError, session.rebind.error) ?? cloud.error} commands={commands} customCommands={customCommands} skills={offered} />
      <WorkspaceComposer
        ipc={ipc}
        workspaceId={workspaceId}
        readOnly={handoff.offLaptop && !cloudMode}
        acceptImages={!cloudMode}
        planMode={commands.planMode}
        agentPicker={<AgentChip selection={session.selection} agentRef={commands.agentRef} disabledHint={agentHint} />}
        {...modelBinding(model, cloudMode, commands.modelRef)}
        runLocation={<RunLocationChip handoff={handoff} onOpenInChat={openInChat} />}
        contextMeter={<ComposerMeters meter={meter} usages={usages} projectId={projectId} />}
        onPlanModeChange={commands.setPlanMode}
        skills={offered}
        customCommands={customCommands}
        onCommand={session.run}
        onFocus={onFocus}
        {...(cloudMode ? cloudBinding(cloud) : local)}
      />
    </>
  );
}
