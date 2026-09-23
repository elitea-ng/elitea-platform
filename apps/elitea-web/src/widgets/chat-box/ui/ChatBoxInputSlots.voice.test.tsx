/**
 * #932/#934 — the two `VoiceButton` callbacks the chat surface never connected.
 */
import { describe, expect, it } from 'vitest';
import { act, renderHook, screen } from '@testing-library/react';
import type { ReactElement } from 'react';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { useChatBoxVoiceFeedback } from './ChatBoxInputSlots';

describe('useChatBoxVoiceFeedback', () => {
  it('issue 932: tracks the dictation recording flag the composer needs', () => {
    const { result } = renderHook(() => useChatBoxVoiceFeedback());
    expect(result.current.isRecording).toBe(false);

    act(() => result.current.onRecordingChange(true));
    expect(result.current.isRecording).toBe(true);

    act(() => result.current.onRecordingChange(false));
    expect(result.current.isRecording).toBe(false);
  });

  it('issue 934: surfaces a mic error as a live-region alert', () => {
    const { result } = renderHook(() => useChatBoxVoiceFeedback());
    act(() => result.current.onError('Microphone access denied.'));

    renderWithTheme(result.current.alert as ReactElement);

    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent('Microphone access denied.');
    expect(screen.getByTestId('chat-voice-error')).toBeInTheDocument();
  });

  it('issue 934: a new recording session clears the previous session error', () => {
    const { result } = renderHook(() => useChatBoxVoiceFeedback());
    act(() => result.current.onError('No microphone found.'));

    act(() => result.current.onRecordingChange(true));
    renderWithTheme(result.current.alert as ReactElement);

    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
