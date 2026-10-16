/**
 * Authored (static) pipeline pauses: the durable REST continuation state for
 * the inline cards `ApplicationAnswer` renders in its approval area (one card
 * per pause, in the spawning instance's accordion). Mounting submits no work.
 *
 * Each card continues on its own: a root pause by itself, a saved-pipeline
 * leaf as a one-leaf `static_decisions` set, so siblings stay answerable.
 * The text is the fixed "Continue" the previous control defaulted to — no
 * prefilled text box.
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
  return useMemo(() => (pause ? { pause, busy, error, onContinue } : undefined), [pause, busy, error, onContinue]);
}
