/**
 * `/retry`, Continue and `/steps`, run by the thread's composer.
 *
 * - `/retry` (or a failed turn's Retry, `SessionCommands.retryRequest`)
 *   sends the person's last message here again, as it was sent (its
 *   "@" references, its skills or custom command), without its images: their
 *   tokens were spent by the first send. With no message sent in this session
 *   it sends the thread's last recorded prompt. The host retries a failed
 *   model call 25 times by itself; `/retry` is for after it gave up.
 * - Continue (a turn stopped at the 512 tool calls one turn can save) starts
 *   a new turn from the host's continue prompt, with a new budget.
 * - Neither goes out while a turn runs or the composer may not send (no
 *   agent, the thread away): a notice says why.
 * - `/steps` shows the folder's step limit; `/steps <number|unlimited|default>`
 *   sets it from the next message (the workspace's agent settings).
 */
import { useEffect, useRef } from 'react';

import { describeWorkspaceError, type WorkspaceCommandId } from '@/features/workspace';
import { DEFAULT_STEP_LIMIT, MAX_STEP_LIMIT, readStepLimit, writeStepLimit, type StepLimit } from '@/shared/desktop/stepLimit';
import type { TurnCommand, TurnStartRequest } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import type { SessionCommands } from './useSessionCommands';
import type { StepsCommand, WorkspaceComposerProps } from './WorkspaceComposer';

/** One send as the composer made it (images aside). */
export interface SentMessage {
  prompt: string;
  mentions: string[];
  skills: TurnStartRequest['skills'];
  command?: TurnCommand | undefined;
}

/** The host's continue prompt (`elitea_agent_runtime::step_limit::CONTINUE_PROMPT`). */
export const CONTINUE_PROMPT = 'Continue from where you stopped.';

/** What a request may send, and when. */
export interface Sending {
  /** Where a message goes now (local, cloud copy or pipeline run). */
  send: WorkspaceComposerProps['onSend'];
  busy: boolean;
  /** The composer may send (an agent picked, the thread here). */
  canSend: boolean;
}

export interface RetryAndSteps {
  /** Wraps the composer's send: the message is remembered for `/retry`. */
  remembering: (send: WorkspaceComposerProps['onSend']) => WorkspaceComposerProps['onSend'];
  /** The composer's `onCommand`: `/steps` here, the rest (`/retry` too) to `run`. */
  onCommand: (command: WorkspaceCommandId) => void;
  onStepsCommand: (command: StepsCommand) => void;
}

function limitText(limit: StepLimit | null): string {
  if (limit === null) return t('workspace.steps.default', 'Each run here takes up to {{steps}} model steps (the default).', { steps: String(DEFAULT_STEP_LIMIT) });
  if (limit === 'unlimited') return t('workspace.steps.unlimited', 'Runs here have no step limit.');
  return t('workspace.steps.limit', 'Each run here takes up to {{steps}} model steps.', { steps: String(limit) });
}

const HOW = (): string => t('workspace.steps.how', 'Set it with /steps <number>, /steps unlimited or /steps default.');

/** Why a request cannot go out now, or null. */
function blocked({ busy, canSend }: Sending): string | null {
  if (busy) return t('workspace.retry.busy', 'A turn is still running here. Try again once it ends.');
  if (!canSend) return t('workspace.retry.cannotSend', 'Nothing can be sent here now. Pick an agent, or bring the thread back to this computer.');
  return null;
}

/** Answer each bump of `request` once, with the latest `answer` (it sees the current send and message). */
function useRequest(request: number, answer: () => void): void {
  const latest = useRef(answer);
  latest.current = answer;
  useEffect(() => {
    if (request > 0) latest.current();
  }, [request]);
}

/**
 * `workspaceId`: the folder whose step limit `/steps` reads and sets. `recorded`: the thread's last recorded message,
 * for `/retry` before any send here.
 */
export function useRetryAndSteps(workspaceId: string, commands: SessionCommands, run: (command: WorkspaceCommandId) => void, sending: Sending, recorded: SentMessage | null): RetryAndSteps {
  const last = useRef<SentMessage | null>(null);

  /** Send `message` unless something stops it, which a notice then names. */
  const sendNow = (message: SentMessage | null): void => {
    const why = message === null ? t('workspace.retryNothing', 'There is no message here to send again.') : blocked(sending);
    if (why !== null || message === null) {
      commands.showText(why ?? '');
      return;
    }
    void sending.send(message.prompt, message.mentions, message.skills, message.command);
  };

  useRequest(commands.retryRequest, () => sendNow(last.current ?? recorded));
  useRequest(commands.continueRequest, () => sendNow({ prompt: CONTINUE_PROMPT, mentions: [], skills: [] }));

  const showSteps = (): void => {
    readStepLimit(workspaceId).then(
      (limit) => commands.showText(`${limitText(limit)} ${HOW()}`),
      (error: unknown) => commands.showText(describeWorkspaceError(error)),
    );
  };

  const onStepsCommand = (command: StepsCommand): void => {
    if ('invalid' in command) {
      commands.showText(t('workspace.steps.invalid', 'A step limit is a whole number from 1 to {{max}}, unlimited or default.', { max: String(MAX_STEP_LIMIT) }));
      return;
    }
    writeStepLimit(workspaceId, command.limit).then(
      (limit) => commands.showText(`${limitText(limit)} ${t('workspace.steps.fromNext', 'It applies from your next message.')}`),
      (error: unknown) => commands.showText(describeWorkspaceError(error)),
    );
  };

  return {
    remembering: (inner) => async (prompt, mentions, skills, command, images) => {
      const sent = await inner(prompt, mentions, skills, command, images);
      if (sent) last.current = { prompt, mentions, skills, command };
      return sent;
    },
    onCommand: (command) => {
      if (command === 'steps') showSteps();
      else run(command);
    },
    onStepsCommand,
  };
}
