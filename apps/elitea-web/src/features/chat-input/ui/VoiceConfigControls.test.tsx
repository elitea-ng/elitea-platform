import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { createTestSocketClient, type TestSocketClient } from '@/shared/api/socket/testing';

import type { TtsVoice } from '../api/ttsVoices';
import type { TtsModel } from '../lib/hooks/useTextToSpeech.types';
import type { VoiceConfig } from '../lib/hooks/useVoiceConfig.hooks';

import { VoiceConfigControls } from './VoiceConfigControls';

const DEFAULT_CONFIG: VoiceConfig = { voiceName: null, voiceId: null, rate: 1.0, volume: 1.0 };
const TTS_MODEL: TtsModel = { id: 'p1_model', name: 'model', project_id: 'p1' };

function browserVoice(name: string, localService = true): SpeechSynthesisVoice {
  return { name, lang: 'en-US', localService, default: false, voiceURI: name };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('VoiceConfigControls', () => {
  it('renders both the speed and volume sliders with their current values', () => {
    const { getAllByRole } = renderWithTheme(
      <VoiceConfigControls
        config={{ ...DEFAULT_CONFIG, rate: 1.5, volume: 0.4 }}
        onConfigChange={() => {}}
        hasModelTTS={false}
        ttsModel={null}
        socket={null}
        browserVoices={[]}
        voices={[]}
      />,
    );
    const sliders = getAllByRole('slider');
    expect(sliders).toHaveLength(2);
    expect(sliders[0]).toHaveAttribute('aria-valuenow', '1.5');
    expect(sliders[1]).toHaveAttribute('aria-valuenow', '0.4');
  });

  it('renders no voice select when there are no voices', () => {
    const { queryByRole } = renderWithTheme(
      <VoiceConfigControls
        config={DEFAULT_CONFIG}
        onConfigChange={() => {}}
        hasModelTTS={false}
        ttsModel={null}
        socket={null}
        browserVoices={[]}
        voices={[]}
      />,
    );
    expect(queryByRole('combobox')).not.toBeInTheDocument();
  });

  it('hides the preview button when isPlaying is true', () => {
    const { queryByTestId } = renderWithTheme(
      <VoiceConfigControls
        config={DEFAULT_CONFIG}
        onConfigChange={() => {}}
        hasModelTTS={false}
        ttsModel={null}
        socket={null}
        browserVoices={[]}
        voices={[]}
        isPlaying
      />,
    );
    expect(queryByTestId('voice-preview-button')).not.toBeInTheDocument();
  });

  it('browser backend: selecting a voice calls onConfigChange with voiceName set and voiceId cleared', async () => {
    const user = userEvent.setup();
    const onConfigChange = vi.fn();
    const voices: SpeechSynthesisVoice[] = [browserVoice('Alpha'), browserVoice('Beta', false)];
    const { getByRole } = renderWithTheme(
      <VoiceConfigControls
        config={DEFAULT_CONFIG}
        onConfigChange={onConfigChange}
        hasModelTTS={false}
        ttsModel={null}
        socket={null}
        browserVoices={voices}
        voices={voices}
      />,
    );

    await user.click(getByRole('combobox'));
    // Beta is not a localService voice — the baseline appends "(online)".
    await user.click(getByRole('option', { name: 'Beta (online)' }));

    expect(onConfigChange).toHaveBeenCalledWith({ voiceName: 'Beta', voiceId: null });
  });

  it('model backend: selecting a voice calls onConfigChange with voiceId set and voiceName cleared', async () => {
    const user = userEvent.setup();
    const onConfigChange = vi.fn();
    const client: TestSocketClient = createTestSocketClient();
    const voices: TtsVoice[] = [{ id: 'v-1', name: 'Server Voice One' }];
    const { getByRole } = renderWithTheme(
      <VoiceConfigControls
        config={DEFAULT_CONFIG}
        onConfigChange={onConfigChange}
        hasModelTTS
        ttsModel={TTS_MODEL}
        socket={client}
        browserVoices={[]}
        voices={voices}
      />,
    );

    await user.click(getByRole('combobox'));
    await user.click(getByRole('option', { name: 'Server Voice One' }));

    expect(onConfigChange).toHaveBeenCalledWith({ voiceId: 'v-1', voiceName: null });
  });

  it('clicking Preview Voice speaks VOICE_PREVIEW_TEXT through the browser engine when no model backend is active', async () => {
    const user = userEvent.setup();
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

    const { getByTestId } = renderWithTheme(
      <VoiceConfigControls
        config={DEFAULT_CONFIG}
        onConfigChange={() => {}}
        hasModelTTS={false}
        ttsModel={null}
        socket={null}
        browserVoices={[]}
        voices={[]}
      />,
    );

    await user.click(getByTestId('voice-preview-button'));
    expect(speak).toHaveBeenCalledOnce();
  });

  /**
   * The picker had NO source (issue 323): `GET /configurations/tts_voices`
   * answered 501 for every project, so `voices` was permanently empty in model
   * mode and the branch below never rendered. These two cases pin the shape the
   * listing now arrives in.
   */
  describe('the provider voice list (issue 323)', () => {
    const PROVIDER_VOICES: TtsVoice[] = [
      { id: 'alloy', name: 'Alloy' },
      { id: 'shimmer', name: 'Shimmer' },
      { id: 'verse', name: 'Verse' },
    ];

    it('offers every provider voice, in the order the provider listed them', async () => {
      const user = userEvent.setup();
      const { getByRole, findAllByRole } = renderWithTheme(
        <VoiceConfigControls
          config={DEFAULT_CONFIG}
          onConfigChange={() => {}}
          hasModelTTS
          ttsModel={TTS_MODEL}
          socket={null}
          browserVoices={[]}
          voices={PROVIDER_VOICES}
        />,
      );

      await user.click(getByRole('combobox'));
      const options = await findAllByRole('option');
      // The FIRST entry is the provider's default, and nothing on this path
      // re-sorts: a sort here would silently change which voice a user gets by
      // leaving the picker untouched.
      expect(options.map((option) => option.textContent)).toEqual(['Alloy', 'Shimmer', 'Verse']);
    });

    it('shows the Default placeholder until a voice is chosen', () => {
      const { getByRole } = renderWithTheme(
        <VoiceConfigControls
          config={DEFAULT_CONFIG}
          onConfigChange={() => {}}
          hasModelTTS
          ttsModel={TTS_MODEL}
          socket={null}
          browserVoices={[]}
          voices={PROVIDER_VOICES}
        />,
      );
      expect(getByRole('combobox')).toHaveTextContent('Default');
    });

    it('renders no picker when the provider publishes no catalogue, so the model keeps its own default', () => {
      // The empty answer is a fact about the PROVIDER now, not about this
      // platform, and the correct response to it is the same: no picker, and
      // the model speaks with the voice it chooses.
      const { queryByRole } = renderWithTheme(
        <VoiceConfigControls
          config={DEFAULT_CONFIG}
          onConfigChange={() => {}}
          hasModelTTS
          ttsModel={TTS_MODEL}
          socket={null}
          browserVoices={[browserVoice('Samantha')]}
          voices={[]}
        />,
      );
      expect(queryByRole('combobox')).not.toBeInTheDocument();
    });
  });
});
