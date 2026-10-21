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
 * once a model call reported its occupancy; `/compact` marks the next send.
 * And the model chip (`ModelChip`, `useTurnModel`): the model the next turn
 * runs on, unless the agent pins it or the folder's settings keep it
 * (`/model` opens it, `/model <name>` sets it).
 */
import { lazy, Suspense } from 'react';

import { useNavigate } from '@tanstack/react-router';

import { ModelChip, type WorkspaceTurn } from '@/features/workspace';
import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import { AgentChip, RunLocationChip } from './ComposerControls';
import { HandoffBanner } from './HandoffControls';
import { firstError, SessionNotices } from './sessionParts';
import type { ComposerSkill } from './useAgentSkills';
import type { CloudChat } from './useCloudChat';
import type { ThreadHandoff } from './useThreadHandoff';
import type { ThreadSession } from './useThreadSession';
import type { SessionCommands } from './useSessionCommands';
import { useTurnModel, type TurnModelControl } from './useTurnModel';
import { WorkspaceComposer, type WorkspaceComposerProps } from './WorkspaceComposer';

const ContextMeter = lazy(() => import('./ContextMeter'));

export interface ThreadComposerProps {
  ipc: WorkspaceIpc;
  workspaceId: string;
  /** The folder's project: the model chip offers its chat models. */
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
    modelPicker: <ModelChip current={model.current} effort={model.effort} models={model.models} onChoose={model.choose} chipRef={chipRef} />,
    onModelCommand: (name) => model.choose({ name }),
  };
}

/** The latest context occupancy of the shown turns: the active one's, else the newest earlier one's. */
function latestMeter(turn: WorkspaceTurn) {
  return turn.view.context ?? turn.earlier.findLast((earlier) => earlier.view.context !== undefined)?.view.context;
}

export function ThreadComposer({ ipc, workspaceId, projectId, conversationId, session, turn, handoff, cloud, skills }: ThreadComposerProps): React.JSX.Element {
  const navigate = useNavigate();
  const { commands } = session;
  const cloudMode = handoff.status?.executor === 'cloud';
  const model = useTurnModel(workspaceId, projectId, session.raw, turn, session.history);
  const local: Pick<WorkspaceComposerProps, 'canSend' | 'busy' | 'onSend' | 'onStop'> = {
    canSend: session.canSend && !handoff.offLaptop,
    busy: turn.busy,
    onSend: async (prompt, mentions, picked) => {
      const sent = await session.send(prompt, commands.planMode, mentions, picked, commands.compactNext, model.choice);
      if (sent && commands.compactNext) commands.compactSent();
      return sent;
    },
    onStop: () => void turn.cancel(),
  };
  const openInChat = conversationId === '' ? undefined : () => void navigate({ to: '/chat/$conversationId', params: { conversationId } });
  const meter = cloudMode ? undefined : latestMeter(turn);
  const agentHint = cloudMode ? t('workspace.cloud.agentHint', 'The cloud copy answers with this thread’s own agent.') : undefined;
  return (
    <>
      <HandoffBanner handoff={handoff} />
      <SessionNotices error={firstError(turn.startError, session.sendError, session.rebind.error) ?? cloud.error} commands={commands} />
      <WorkspaceComposer
        ipc={ipc}
        workspaceId={workspaceId}
        readOnly={handoff.offLaptop && !cloudMode}
        planMode={commands.planMode}
        agentPicker={<AgentChip selection={session.selection} agentRef={commands.agentRef} disabledHint={agentHint} />}
        {...modelBinding(model, cloudMode, commands.modelRef)}
        runLocation={<RunLocationChip handoff={handoff} onOpenInChat={openInChat} />}
        contextMeter={
          meter === undefined ? undefined : (
            <Suspense fallback={null}>
              <ContextMeter meter={meter} />
            </Suspense>
          )
        }
        onPlanModeChange={commands.setPlanMode}
        skills={skills}
        onCommand={session.run}
        onFocus={session.prewarm}
        {...(cloudMode ? cloudBinding(cloud) : local)}
      />
    </>
  );
}
