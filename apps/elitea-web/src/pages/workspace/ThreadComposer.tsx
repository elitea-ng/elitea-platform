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
 */
import { useNavigate } from '@tanstack/react-router';

import type { WorkspaceTurn } from '@/features/workspace';
import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import { AgentChip, RunLocationChip } from './ComposerControls';
import { HandoffBanner } from './HandoffControls';
import { firstError, SessionNotices } from './sessionParts';
import type { ComposerSkill } from './useAgentSkills';
import type { CloudChat } from './useCloudChat';
import type { ThreadHandoff } from './useThreadHandoff';
import type { ThreadSession } from './useThreadSession';
import { WorkspaceComposer, type WorkspaceComposerProps } from './WorkspaceComposer';

export interface ThreadComposerProps {
  ipc: WorkspaceIpc;
  workspaceId: string;
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

export function ThreadComposer({ ipc, workspaceId, conversationId, session, turn, handoff, cloud, skills }: ThreadComposerProps): React.JSX.Element {
  const navigate = useNavigate();
  const { commands } = session;
  const cloudMode = handoff.status?.executor === 'cloud';
  const local: Pick<WorkspaceComposerProps, 'canSend' | 'busy' | 'onSend' | 'onStop'> = {
    canSend: session.canSend && !handoff.offLaptop,
    busy: turn.busy,
    onSend: (prompt, mentions, picked) => session.send(prompt, commands.planMode, mentions, picked),
    onStop: () => void turn.cancel(),
  };
  const openInChat = conversationId === '' ? undefined : () => void navigate({ to: '/chat/$conversationId', params: { conversationId } });
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
        runLocation={<RunLocationChip handoff={handoff} onOpenInChat={openInChat} />}
        onPlanModeChange={commands.setPlanMode}
        skills={skills}
        onCommand={session.run}
        onFocus={session.prewarm}
        {...(cloudMode ? cloudBinding(cloud) : local)}
      />
    </>
  );
}
