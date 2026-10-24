/**
 * `/retry` and `/steps`, run by the thread's composer.
 *
 * - `/retry` (or a failed turn's Retry, `SessionCommands.retryRequest`)
 *   sends the person's last message here again, as it was sent (its
 *   "@" references, its skills or custom command), without its images: their
 *   tokens were spent by the first send. With no message sent in this session
 *   it sends the thread's last recorded prompt. The host retries a failed
 *   model call 25 times by itself; `/retry` is for after it gave up.
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

export interface RetryAndSteps {
  /** Wraps the composer's send: the message is remembered for `/retry`. */
  remembering: (send: WorkspaceComposerProps['onSend']) => WorkspaceComposerProps['onSend'];
  /** The composer's `onCommand`: `/steps` here, the rest (`/retry` too) to `run`. */
  onCommand: (command: WorkspaceCommandId) => void;
  onStepsCommand: (command: StepsCommand) => void;
}

function limitText(limit: StepLimit | null): string {
  if (limit === null) return t('workspace.steps.default', 'Each run here takes up to {{steps}} model steps (the default), or the agent’s own limit when that is higher.', { steps: String(DEFAULT_STEP_LIMIT) });
  if (limit === 'unlimited') return t('workspace.steps.unlimited', 'Runs here have no step limit.');
  return t('workspace.steps.limit', 'Each run here takes up to {{steps}} model steps.', { steps: String(limit) });
}

const HOW = (): string => t('workspace.steps.how', 'Set it with /steps <number>, /steps unlimited or /steps default.');

/**
 * `workspaceId`: the folder whose step limit `/steps` reads and sets. `send`: where a message goes now (local, cloud
 * copy or pipeline run). `recorded`: the thread's last recorded prompt, for `/retry` before any send here.
 */
export function useRetryAndSteps(workspaceId: string, commands: SessionCommands, run: (command: WorkspaceCommandId) => void, send: WorkspaceComposerProps['onSend'], busy: boolean, recorded: SentMessage | null): RetryAndSteps {
  const last = useRef<SentMessage | null>(null);

  const retry = (): void => {
    if (busy) return;
    const message = last.current ?? recorded;
    if (message === null) {
      commands.showNothingToRetry();
      return;
    }
    void send(message.prompt, message.mentions, message.skills, message.command);
  };

  // A request (`/retry`, a failed turn's Retry) is answered once; the latest
  // `retry` is the one that sees the current send and message.
  const latest = useRef(retry);
  latest.current = retry;
  const request = commands.retryRequest;
  useEffect(() => {
    if (request > 0) latest.current();
  }, [request]);

  const showSteps = (): void => {
    readStepLimit(workspaceId).then(
      (limit) => commands.showSteps(`${limitText(limit)} ${HOW()}`),
      (error: unknown) => commands.showSteps(describeWorkspaceError(error)),
    );
  };

  const onStepsCommand = (command: StepsCommand): void => {
    if ('invalid' in command) {
      commands.showSteps(t('workspace.steps.invalid', 'A step limit is a whole number from 1 to {{max}}, unlimited or default.', { max: String(MAX_STEP_LIMIT) }));
      return;
    }
    writeStepLimit(workspaceId, command.limit).then(
      (limit) => commands.showSteps(`${limitText(limit)} ${t('workspace.steps.fromNext', 'It applies from your next message.')}`),
      (error: unknown) => commands.showSteps(describeWorkspaceError(error)),
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
