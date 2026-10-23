/**
 * Authored (static) pipeline pauses: the durable REST continuation state for
 * the inline cards `ApplicationAnswer` renders in its approval area (one card
 * per pause, in the spawning instance's accordion). Mounting submits no work.
 *
 * Each card continues on its own: a root pause by itself, a saved-pipeline
 * leaf as a one-leaf `static_decisions` set, so siblings stay answerable.
 * The text is the fixed "Continue" the previous control defaulted to — no
 * prefilled text box. A message typed in the chat input while a pause is
 * current continues it with that text instead (every paused pipeline of a
 * saved-pipeline set), never as a new turn.
 */
import { useCallback, useMemo } from 'react';

import { t } from '@/shared/i18n';
import type { StaticPauseBinding } from '@/features/chat-messages';

import { useStaticPipelineContinuation, type StaticContinuationParams } from './hooks/useStaticPipelineContinuation';
import type { ChatBoxProps } from './ChatBox.types';

type HostProps = Omit<StaticContinuationParams, 'restoredRun'> & {
  readonly editorTest: NonNullable<ChatBoxProps['extensions']>['editorTest'];
};

/** What the message list needs to render the current pause's cards. */
export interface StaticPauseCardsBinding {
  readonly pause: StaticPauseBinding;
  readonly busy: boolean;
  readonly error?: string | undefined;
  readonly onContinue: (pauseId: string | undefined) => void;
  /** Continue the current pause with a message typed in the chat input; `true` once it started. */
  readonly onSendText: (text: string) => Promise<boolean>;
}

/**
 * Continue the current pause with typed text, clearing the chat input only once
 * the continuation has started: a refused or unavailable continuation keeps the
 * text where the user typed it.
 */
export async function sendTypedStaticContinuation(
  binding: StaticPauseCardsBinding,
  text: string,
  resetInput: () => void,
): Promise<void> {
  if (await binding.onSendText(text)) resetInput();
}

/** Shared composition for main chat and Pipeline Test. */
export function useStaticPauseCards({ editorTest, ...params }: HostProps): StaticPauseCardsBinding | undefined {
  const controls = useStaticPipelineContinuation({ ...params, restoredRun: editorTest?.restoredRun });
  const { submit, pause, busy, error } = controls;
  const onContinue = useCallback(
    (pauseId: string | undefined) => {
      void submit(t('widgets.chatBox.staticPause.defaultMessage', 'Continue'), pauseId ? [pauseId] : []);
    },
    [submit],
  );
  const onSendText = useCallback(
    (text: string) =>
      pause
        ? submit(text, pause.kind === 'root' ? [] : pause.inventory.pauses.map((leaf) => leaf.proof.pause_id))
        : Promise.resolve(false),
    [submit, pause],
  );
  return useMemo(
    () => (pause ? { pause, busy, error, onContinue, onSendText } : undefined),
    [pause, busy, error, onContinue, onSendText],
  );
}
