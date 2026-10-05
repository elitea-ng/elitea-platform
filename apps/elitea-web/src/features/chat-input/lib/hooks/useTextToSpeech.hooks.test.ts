import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { voiceTransportMock } from '../__mocks__/voiceTransport.mock';

import { useTextToSpeech } from './useTextToSpeech.hooks';
import type { TtsModel } from './useTextToSpeech.types';

const synthesizeSpeech = voiceTransportMock.synthesizeSpeech;

const TTS_MODEL: TtsModel = { id: 'p1_voice-model', name: 'voice-model', project_id: 'p1' };

describe('useTextToSpeech — backend selection', () => {
  let originalAudioContext: typeof window.AudioContext | undefined;

  beforeEach(() => {
    originalAudioContext = window.AudioContext;
    synthesizeSpeech.mockReset();
    // A request that never answers: these tests observe the dispatch only.
    synthesizeSpeech.mockReturnValue(new Promise(() => {}));
  });

  afterEach(() => {
    window.AudioContext = originalAudioContext as typeof window.AudioContext;
    vi.unstubAllGlobals();
  });

  it('with no ttsModel/project/AudioContext, falls back to the browser engine (isSupported reflects speechSynthesis only)', () => {
    Reflect.deleteProperty(window, 'AudioContext');
    Reflect.deleteProperty(window, 'speechSynthesis');
    const { result } = renderHook(() => useTextToSpeech({}));
    expect(result.current.isSupported).toBe(false);
  });

  it('isSupported is true when speechSynthesis exists, even with no model', () => {
    Reflect.deleteProperty(window, 'AudioContext');
    vi.stubGlobal('speechSynthesis', { speak: vi.fn(), cancel: vi.fn() });
    const { result } = renderHook(() => useTextToSpeech({}));
    expect(result.current.isSupported).toBe(true);
  });

  it("speak() with nothing able to speak reports 'no-model' instead of arming a silent player", () => {
    Reflect.deleteProperty(window, 'AudioContext');
    Reflect.deleteProperty(window, 'speechSynthesis');
    const onError = vi.fn();
    const { result } = renderHook(() => useTextToSpeech({ onError }));

    act(() => result.current.speak('Hello'));

    expect(onError).toHaveBeenCalledWith('no-model');
    expect(result.current.isPlaying).toBe(false);
  });

  /** A minimally-functional `AudioContext` stand-in — `useModelTtsEngine`'s `speak()` calls `createGain()`/`.connect()` synchronously, so `isAudioContextSupported()` alone (an empty class) is not enough here, unlike the "falls back" tests below which never get that far. */
  class MinimalFakeAudioContext {
    state = 'running';
    currentTime = 0;
    createGain(): { gain: { value: number }; connect: () => void } {
      return { gain: { value: 1 }, connect: () => {} };
    }
    close(): Promise<void> {
      this.state = 'closed';
      return Promise.resolve();
    }
  }

  it('with a ttsModel + project + AudioContext all present, speak() drives the MODEL engine (one HTTPS speech request)', () => {
    vi.stubGlobal('AudioContext', MinimalFakeAudioContext);
    const { result } = renderHook(() => useTextToSpeech({ ttsModel: TTS_MODEL, projectId: '7', voiceConfig: {} }));

    act(() => result.current.speak('Hello'));

    expect(synthesizeSpeech).toHaveBeenCalledOnce();
    expect(synthesizeSpeech.mock.calls[0]?.[0]).toMatchObject({ projectId: '7', model: 'voice-model', input: 'Hello' });
    expect(result.current.isPlaying).toBe(true);
  });

  it('missing project (even with a ttsModel + AudioContext) falls back to the browser engine — no speech request', () => {
    vi.stubGlobal('AudioContext', MinimalFakeAudioContext);
    const speak = vi.fn();
    vi.stubGlobal('speechSynthesis', { speak, cancel: vi.fn() });
    vi.stubGlobal(
      'SpeechSynthesisUtterance',
      class {
        text: string;
        constructor(text: string) {
          this.text = text;
        }
      },
    );

    const { result } = renderHook(() => useTextToSpeech({ ttsModel: TTS_MODEL, voiceConfig: {} }));
    act(() => result.current.speak('Hello'));

    expect(speak).toHaveBeenCalledOnce();
    expect(synthesizeSpeech).not.toHaveBeenCalled();
  });

  it('speak("") is a no-op regardless of backend', () => {
    const { result } = renderHook(() => useTextToSpeech({}));
    act(() => result.current.speak(''));
    expect(result.current.isPlaying).toBe(false);
  });

  it('setShowPlayer/setSpeakableText/speakableText round-trip (the UI-facing player state this hook owns)', () => {
    const { result } = renderHook(() => useTextToSpeech({}));
    act(() => {
      result.current.setSpeakableText('spoken text');
      result.current.setShowPlayer(true);
    });
    expect(result.current.speakableText).toBe('spoken text');
    expect(result.current.showPlayer).toBe(true);
  });

  it('stop() resets speakableText/showPlayer, matching baseline\'s unconditional resetStatus("idle") — an explicit Stop must not leave stale text for a later onPlay to silently replay', () => {
    vi.stubGlobal('speechSynthesis', { speak: vi.fn(), cancel: vi.fn() });
    vi.stubGlobal(
      'SpeechSynthesisUtterance',
      class {
        text: string;
        constructor(text: string) {
          this.text = text;
        }
      },
    );
    const { result } = renderHook(() => useTextToSpeech({}));
    act(() => {
      result.current.setSpeakableText('spoken text');
      result.current.setShowPlayer(true);
      result.current.speak('spoken text');
    });

    act(() => result.current.stop());

    expect(result.current.speakableText).toBe('');
    expect(result.current.showPlayer).toBe(false);
  });
});
