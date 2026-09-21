/**
 * The player an answer being READ ALOUD is stopped from (issue 974).
 *
 * `useReadAloud` has always returned `showPlayer` — set by `onAutoSpeak`,
 * cleared when playback ends — and this widget destructured it NOWHERE, so the
 * pill the reference SPA renders (`ChatBox.jsx`: `{showPlayer &&
 * <VoiceMiniPlayer {...voicePlayerProps} />}`) had no counterpart here.
 * Pressing "Read out" therefore surfaced no player, and the only control that
 * could stop the voice was one permanently parked in the composer, labelled
 * for the composer's own voice settings.
 *
 * Its own file rather than four lines inside `ChatBox`: that component sits on
 * the §3.5 400-line ceiling and its complexity budget, and one more `&&` in
 * its body costs both.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';

import { VoiceMiniPlayer } from '@/features/chat-input';

/** `voiceHooks.useReadAloud()`'s player half, as this widget consumes it. */
export interface ChatBoxVoicePlayerProps {
  readonly showPlayer: boolean;
  readonly voicePlayerProps: React.ComponentProps<typeof VoiceMiniPlayer>;
}

export function ChatBoxVoicePlayer({ showPlayer, voicePlayerProps }: ChatBoxVoicePlayerProps): ReactNode {
  if (!showPlayer) return null;
  return (
    <Box sx={{ display: 'flex', justifyContent: 'center' }}>
      <VoiceMiniPlayer {...voicePlayerProps} />
    </Box>
  );
}
