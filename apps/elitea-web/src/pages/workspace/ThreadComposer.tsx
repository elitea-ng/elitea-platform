/**
 * The bottom of a thread: the notice for a thread that is away, the errors,
 * and the composer. Where a message goes depends on where the thread runs:
 *
 * - here: the local agent loop (`session.send`), with Act | Plan | Bypass
 *   (`useBypassMode`, kept per folder; Plan wins) and the
 *   agent·version picker;
 * - `cloud`: the SERVER conversation (`useCloudChat`: the chat's own execution
 *   route and stream). The conversation's own agent answers, so the picker is
 *   shown disabled and there is no plan mode;
 * - `returning`: read-only, with its notice.
 *
 * A local thread's footer also shows the context ring (`UsageChip`, lazy: the
 * share of the window in use; its tooltip the latest turn's tokens, the
 * thread's, a cost only from the catalogue's price, #1227); a click opens what
 * fills the context, the inspector's Context tab (`ContextPanel`); `/compact`
 * marks the next send. The host summarizes a thread left idle in the
 * background unless its composer holds unsent text (`useIdleHold`); a notice
 * says when it did (`useIdleCompacted`).
 * Beside it the language-server icon (`LspChip`, lazy, #1236): the servers
 * behind the edits' diagnostics; and the index icon (`IndexStatusIcon`): the folder's index state, a click opens the
 * Index tab. The ⋯ menu holds the step limit
 * (`StepLimitMenu`, lazy): the model steps each run here may take (`/steps`
 * sets the same).
 * `/retry` sends the last message again and `/steps` shows or sets the
 * folder's step limit (`useRetryAndSteps`).
 * A new thread of a git folder chooses where it works (`WorkInChip`, `useWorkIn`): the folder, or a new task worktree
 * that its first send makes and moves to.
 * And the model chip (`ModelChip`, `useTurnModel`): the model the next turn
 * runs on, unless the agent pins it or the folder's settings keep it
 * (`/model` opens it, `/model <name>` sets a listed one; an unknown name or
 * a catalogue still loading is said above the composer and sets nothing).
 *
 * A pipeline (#1239 A3) never runs here: when it is the pick, a send runs it
 * in the cloud and opens its chat (`useRunInCloud`; no plan mode, images or
 * model chip); a start the host refuses with `pipeline_unsupported` offers
 * the same for that prompt (`RunInCloudNotice`).
 *
 * While a local turn runs, a send steers it (`useSteering`): the text waits for the agent's next safe point
 * (`SteeringQueue`, at the top of the composer, with Send now, Edit and Remove), and what the turn did not take goes
 * as the next turn. When that next turn does not start, the texts go back into the composer with "Send again"
 * (`unsent`). Esc Esc stops a running turn, as the composer's Stop does (`useDoubleEscape`).
 */
import { lazy, Suspense, useEffect, useMemo, useState } from 'react';

import { useNavigate } from '@tanstack/react-router';

import { composer, replayView, type WorkspaceTurn } from '@/features/workspace';
import { IndexStatusIcon } from '@/features/workspace-index';
import type { CustomCommand, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { useDoubleEscape } from '@/shared/lib/useDoubleEscape';
import { useDesktopLayout } from '@/widgets/desktop-shell';

import { BypassControls } from './BypassControls';
import { contextBreakdown, threadMeter } from './contextBreakdown';
import { IdleCompactedNotice } from './CompactNotices';
import { AgentChip, RunLocationControl, WorkInChip } from './ComposerControls';
import { HandoffBanner } from './HandoffControls';
import { RunInCloudNotice } from './RunInCloudNotice';
import { firstError, SessionNotices } from './sessionParts';
import { useProjectSkills, type ComposerSkill } from './useAgentSkills';
import type { CloudChat } from './useCloudChat';
import { useCustomCommands } from './useCustomCommands';
import { pipelineComposerBinding, useComposerPipeline, type ComposerPipeline } from './useRunInCloud';
import type { ThreadHandoff } from './useThreadHandoff';
import type { ThreadSession } from './useThreadSession';
import { useRetryAndSteps, type SentMessage } from './useRetryAndSteps';
import type { SessionCommands } from './useSessionCommands';
import { useComposerModes } from './useComposerModes';
import { useIdleCompacted } from './useIdleCompacted';
import { useIdleHold } from './useIdleHold';
import { useSteering, type Steering } from './useSteering';
import { useTurnModel, type TurnModelControl } from './useTurnModel';
import { useWorkIn } from './useWorkIn';
import StepLimitMenu, { prefetchStepLimit } from './StepLimitMenu';
import { skillPicks, WorkspaceComposer, type WorkspaceComposerProps } from './WorkspaceComposer';

const UsageChip = lazy(() => import('./UsageChip'));
const LspChip = lazy(() => import('./LspChip'));

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
  /** The folder is its repository's top (not a task worktree): a new thread may work in a new worktree. */
  canBranch: boolean;
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

type SendBinding = Partial<Pick<WorkspaceComposerProps, 'placeholder' | 'showMode' | 'acceptImages' | 'onSteer' | 'queue' | 'unsent'>> & Pick<WorkspaceComposerProps, 'canSend' | 'busy' | 'onSend' | 'onStop'>;

/**
 * Where a message goes: the cloud copy, else the pipeline's cloud run (gated on that run alone, never on the local
 * session: nothing runs here), else the local agent loop.
 */
function sendBinding(cloudMode: boolean, cloud: CloudChat, pipeline: ComposerPipeline, here: SendBinding): SendBinding {
  if (cloudMode) return cloudBinding(cloud);
  return pipeline.pipeline ? pipelineComposerBinding(pipeline.run) : here;
}

/** The model chip and `/model <name>`: neither for a cloud thread (its own agent answers) nor when no choice is offered. */
function modelBinding(model: TurnModelControl, cloudMode: boolean, chipRef: SessionCommands['modelRef']): Pick<WorkspaceComposerProps, 'modelPicker' | 'onModelCommand'> {
  if (cloudMode || !model.offered) return {};
  return {
    modelPicker: <composer.ModelChip current={model.current} effort={model.effort} models={model.models} onChoose={model.choose} chipRef={chipRef} />,
    onModelCommand: model.command,
  };
}

type Usage = WorkspaceTurn['view']['usage'];

/**
 * The thread's last recorded message (thread history), for `/retry` before anything was sent here. The history keeps
 * the prompt as typed: its skill or custom command is read from it again, as a send does.
 */
export function recordedPrompt(history: ThreadSession['history'], commands: readonly CustomCommand[], skills: readonly ComposerSkill[]): SentMessage | null {
  if (history.kind !== 'local') return null;
  const stored = history.turns[history.turns.length - 1];
  if (stored === undefined) return null;
  const { command, skill } = composer.invokedPick(stored.prompt, commands, skills);
  return { prompt: stored.prompt, mentions: stored.mentions, skills: skillPicks(skill), command: command ?? undefined };
}

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

/** The queue (and its refusals) always; while a local turn runs, Send steers it; after it, the texts that did not start a turn. */
function steeringProps(steering: Steering): Pick<WorkspaceComposerProps, 'onSteer' | 'queue' | 'unsent'> {
  if (steering.active) return { onSteer: steering.steer, queue: steering };
  return steering.unsent === null ? { queue: steering } : { queue: steering, unsent: steering.unsent };
}

/** The local session can start a turn, or the running turn takes a steering message; never while the thread is away. */
const canSendHere = (session: ThreadSession, steering: Steering, offLaptop: boolean): boolean => (session.canSend || steering.active) && !offLaptop;

interface ComposerMetersProps {
  usages: NonNullable<Usage>[];
  projectId: number;
  workspaceId: string;
  /** A local thread: the folder's language servers show (not for the cloud copy). */
  local: boolean;
  /** The share of the model's window in use, once the host measured it. */
  contextPercent: number | undefined;
}

/** The share of the window the thread's latest call used; none for the cloud copy or before the host measured it. */
function contextPercentOf(cloudMode: boolean, turn: WorkspaceTurn, history: ThreadSession['history']): number | undefined {
  const meter = cloudMode ? undefined : threadMeter(turn, history);
  return meter === undefined ? undefined : contextBreakdown(meter).percent;
}

/** "Work in" is a new local thread's choice, in a folder that is its repository's top. */
const offersWorkIn = (canBranch: boolean, local: boolean, conversationId: string): boolean => canBranch && local && conversationId === '';

const openContextTab = (): void => useDesktopLayout.getState().showInspector('context');
const openIndexTab = (): void => useDesktopLayout.getState().showInspector('index');

/** The footer's language-server icon and the context ring (both lazy); the ring once a model call reported usage. */
function ComposerMeters({ usages, projectId, workspaceId, local, contextPercent }: ComposerMetersProps): React.JSX.Element | null {
  if (usages.length === 0 && !local) return null;
  return (
    <Suspense fallback={null}>
      {local ? <LspChip workspaceId={workspaceId} /> : null}
      {local ? <IndexStatusIcon workspaceId={workspaceId} onOpen={openIndexTab} /> : null}
      {usages.length === 0 ? null : <UsageChip turns={usages} projectId={projectId} contextPercent={contextPercent} onOpenContext={openContextTab} />}
    </Suspense>
  );
}

/** The ⋯ menu's rows for a local thread: the step limit (read ahead, so the menu opens whole); a choice it cannot store is said above the composer. */
function moreRows(workspaceId: string, local: boolean, onStepsError: (message: string) => void): WorkspaceComposerProps['more'] {
  if (!local) return undefined;
  return (close) => <StepLimitMenu workspaceId={workspaceId} onError={onStepsError} close={close} />;
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
    // Only the local host reads ahead (not for the cloud copy or a pipeline).
    if (local) prewarm();
  };
  return { customCommands, offered, onFocus };
}

/** The notice of the thread's latest idle compaction, until it is dismissed or a message is sent. */
function useIdleNotice(workspaceId: string, conversationId: string): { shown: boolean; dismiss: () => void } {
  const compacted = useIdleCompacted(workspaceId, conversationId);
  const [dismissed, setDismissed] = useState<string | null>(null);
  const at = compacted?.at ?? null;
  return { shown: at !== null && at !== dismissed, dismiss: () => setDismissed(at) };
}

export function ThreadComposer({ ipc, workspaceId, projectId, conversationId, session, turn, handoff, cloud, skills, canBranch }: ThreadComposerProps): React.JSX.Element {
  const navigate = useNavigate();
  const { commands } = session;
  const cloudMode = handoff.status?.executor === 'cloud';
  useEffect(() => {
    if (!cloudMode) prefetchStepLimit(workspaceId);
  }, [cloudMode, workspaceId]);
  const composerPipeline = useComposerPipeline(projectId, session.raw, turn, cloudMode);
  const { pipeline, run: runInCloud, remember, refusedPrompt, turnError } = composerPipeline;
  const model = useTurnModel(workspaceId, projectId, session.raw, turn, session.history);
  const local = !cloudMode && !pipeline;
  const modes = useComposerModes(workspaceId, local, commands);
  const idle = useIdleNotice(workspaceId, conversationId);
  const onDraftChange = useIdleHold(workspaceId, conversationId, local);
  const { customCommands, offered, onFocus } = useSlashSources(ipc, workspaceId, projectId, local, skills, session.prewarm, commands.notice === 'help');
  const sendHere: WorkspaceComposerProps['onSend'] = async (prompt, mentions, picked, command, images) => {
    model.dismissNotice();
    idle.dismiss();
    remember(prompt);
    const sent = await session.send(prompt, modes.mode, mentions, picked, commands.compactNext, model.choice, command, images);
    if (sent && commands.compactNext) commands.compactSent();
    return sent;
  };
  const steering = useSteering(ipc, turn, (prompt) => sendHere(prompt, [], []));
  const here: SendBinding = {
    canSend: canSendHere(session, steering, handoff.offLaptop),
    busy: turn.busy,
    onSend: sendHere,
    onStop: () => void turn.cancel(),
    ...steeringProps(steering),
  };
  const bound = sendBinding(cloudMode, cloud, composerPipeline, here);
  useDoubleEscape(bound.busy, bound.onStop);
  const lastRecorded = useMemo(() => recordedPrompt(session.history, customCommands, offered), [session.history, customCommands, offered]);
  const controls = useRetryAndSteps(workspaceId, commands, session.run, { send: bound.onSend, busy: bound.busy, canSend: bound.canSend }, lastRecorded);
  const workIn = useWorkIn({
    workspaceId,
    offered: offersWorkIn(canBranch, local, conversationId),
    canSend: bound.canSend,
    send: controls.remembering(bound.onSend),
    onError: commands.showText,
    open: (id) => void navigate({ to: '/workspaces/$workspaceId', params: { workspaceId: id } }),
  });
  const openInChat = conversationId === '' ? undefined : () => void navigate({ to: '/chat/$conversationId', params: { conversationId } });
  const recorded = useMemo(() => recordedUsages(session.history), [session.history]);
  const usages = cloudMode ? NO_USAGE : turnUsages(turn, recorded);
  const modelNotice = cloudMode ? null : model.notice;
  const agentHint = cloudMode ? t('workspace.cloud.agentHint', 'The cloud copy answers with this thread’s own agent.') : undefined;
  return (
    <>
      <HandoffBanner handoff={handoff} projectId={projectId} />
      <SessionNotices error={modelNotice ?? firstError(turnError, session.sendError, session.rebind.error) ?? cloud.error} commands={commands} customCommands={customCommands} skills={offered} />
      <BypassControls bypass={modes.bypass} active={modes.bypassActive} />
      {local && <IdleCompactedNotice shown={idle.shown} onClose={idle.dismiss} />}
      <RunInCloudNotice cloudOnly={pipeline} refusedPrompt={refusedPrompt} running={runInCloud.running} error={runInCloud.error} onRun={(prompt) => void runInCloud.run(prompt)} />
      <WorkspaceComposer
        ipc={ipc}
        workspaceId={workspaceId}
        conversationId={conversationId}
        readOnly={handoff.offLaptop && !cloudMode}
        acceptImages={!cloudMode}
        planMode={commands.planMode}
        {...modes.switchProps}
        agentPicker={<AgentChip selection={session.selection} agentRef={commands.agentRef} disabledHint={agentHint} />}
        {...modelBinding(model, !local, commands.modelRef)}
        runLocation={
          <>
            {workIn.offered && <WorkInChip value={workIn.value} onChange={workIn.choose} disabled={bound.busy} />}
            <RunLocationControl handoff={handoff} onOpenInChat={openInChat} />
          </>
        }
        contextMeter={<ComposerMeters usages={usages} projectId={projectId} workspaceId={workspaceId} local={!cloudMode} contextPercent={contextPercentOf(cloudMode, turn, session.history)} />}
        more={moreRows(workspaceId, !cloudMode, commands.showText)}
        onPlanModeChange={commands.setPlanMode}
        skills={offered}
        customCommands={customCommands}
        onCommand={controls.onCommand}
        onStepsCommand={controls.onStepsCommand}
        onFocus={onFocus}
        onDraftChange={onDraftChange}
        {...bound}
        onSend={workIn.send}
      />
    </>
  );
}
