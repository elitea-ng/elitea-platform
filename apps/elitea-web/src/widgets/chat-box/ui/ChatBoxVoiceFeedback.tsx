import type { ReactNode } from 'react';
import { useCallback, useState } from 'react';

import Alert from '@mui/material/Alert';
import Snackbar from '@mui/material/Snackbar';

/**
 * The composer's voice-control feedback seam (issues #932, #934).
 *
 * `VoiceButton` has always exposed two callbacks the chat surface never
 * connected:
 *
 *  - `onRecordingChange` — wired to `() => {}` in `ChatBoxInputSlots.tsx`, so
 *    `NewChatInput`'s `finalIsRecording = voice.isRecording ||
 *    isSpeakingModeRecording` was driven by speaking mode alone. Starting
 *    dictation never reached the send control, which is why the Speaking Mode
 *    wave icon stayed live while the mic was recording and a user could
 *    enter both modes at once (#932, ELITEA-1295).
 *  - `onError` — passed nothing at all. `VoiceButton`'s own module doc
 *    disclosed the gap ("no toast/snackbar primitive exists yet in
 *    `shared/ui`... the caller decides how to surface the message until one
 *    lands") and no caller ever decided, so a denied microphone permission
 *    failed in complete silence (#934, ELITEA-1324).
 *
 * The Snackbar+Alert pair is the same shape `widgets/deepwiki`'s
 * `WikiFileAttach.tsx` already uses for its own inline errors — MUI's `Alert`
 * carries `role="alert"`, so the message is announced, not merely drawn.
 */
const VOICE_ERROR_AUTO_HIDE_MS = 6000;

export interface ChatBoxVoiceFeedback {
  /** True while `VoiceButton`'s dictation (NOT speaking mode) is capturing. */
  readonly isRecording: boolean;
  readonly onRecordingChange: (isRecording: boolean) => void;
  readonly onError: (message: string) => void;
  /** Rendered by `ChatBox` alongside its other overlays. */
  readonly alert: ReactNode;
}

export function useChatBoxVoiceFeedback(): ChatBoxVoiceFeedback {
  const [isRecording, setIsRecording] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const onRecordingChange = useCallback((recording: boolean) => {
    setIsRecording(recording);
    // A new session clears the previous session's complaint; leaving it up
    // would outlive the state it described.
    if (recording) setError(null);
  }, []);

  const onError = useCallback((message: string) => {
    setError(message);
  }, []);

  const onClose = useCallback(() => {
    setError(null);
  }, []);

  const alert = (
    <Snackbar
      open={error !== null}
      autoHideDuration={VOICE_ERROR_AUTO_HIDE_MS}
      onClose={onClose}
    >
      <Alert
        severity="error"
        onClose={onClose}
        data-testid="chat-voice-error"
      >
        {error}
      </Alert>
    </Snackbar>
  );

  return { isRecording, onRecordingChange, onError, alert };
}
