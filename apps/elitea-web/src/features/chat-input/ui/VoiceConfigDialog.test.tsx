import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { installWebStorageShim } from '../../../test/webstorage';

import type { TtsModel } from '../lib/hooks/useTextToSpeech.types';
import type { VoiceConfig } from '../lib/hooks/useVoiceConfig.hooks';

import { VoiceConfigDialog } from './VoiceConfigDialog';

installWebStorageShim();

const CONFIG: VoiceConfig = { voiceName: null, voiceId: null, rate: 1.0, volume: 1.0 };
const TTS_MODEL: TtsModel = { id: 'p1_model', name: 'model', project_id: 'p1' };

describe('VoiceConfigDialog', () => {
  it('renders nothing to the accessibility tree when closed (no dialog role)', () => {
    const { queryByRole } = renderWithTheme(
      <VoiceConfigDialog
        config={CONFIG}
        voices={[]}
        open={false}
        onApply={() => {}}
        onCancel={() => {}}
        ttsModel={null}
        hasModelTTS={false}
      />,
    );
    expect(queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('Cancel calls onCancel without calling onApply', async () => {
    const user = userEvent.setup();
    const onApply = vi.fn();
    const onCancel = vi.fn();
    const { getByRole } = renderWithTheme(
      <VoiceConfigDialog
        config={CONFIG}
        voices={[]}
        open
        onApply={onApply}
        onCancel={onCancel}
        ttsModel={null}
        hasModelTTS={false}
      />,
    );

    await user.click(getByRole('button', { name: 'Cancel' }));
    expect(onCancel).toHaveBeenCalledOnce();
    expect(onApply).not.toHaveBeenCalled();
  });

  it('Apply calls onApply with the staged (locally edited) config, not the original prop', async () => {
    const user = userEvent.setup();
    const onApply = vi.fn();
    const { getByRole, getAllByRole } = renderWithTheme(
      <VoiceConfigDialog
        config={CONFIG}
        voices={[]}
        open
        onApply={onApply}
        onCancel={() => {}}
        ttsModel={null}
        hasModelTTS={false}
      />,
    );

    // Move the volume slider via keyboard (focus + ArrowLeft decrements by
    // `step` — the default config's volume already sits at its max, 1.0, so
    // ArrowRight would be a clamped no-op).
    const sliders = getAllByRole('slider');
    const volumeSlider = sliders[1];
    expect(volumeSlider).toBeDefined();
    if (volumeSlider) {
      volumeSlider.focus();
      await user.keyboard('{ArrowLeft}');
    }

    await user.click(getByRole('button', { name: 'Apply' }));

    expect(onApply).toHaveBeenCalledOnce();
    const applied = onApply.mock.calls[0]?.[0] as VoiceConfig;
    expect(applied.volume).toBeLessThan(CONFIG.volume);
    // The original `config` prop itself is untouched (staged edits are local).
    expect(CONFIG.volume).toBe(1.0);
  });

  it('re-syncs the staged config from the `config` prop every time the dialog re-opens', () => {
    const { rerender, getAllByRole } = renderWithTheme(
      <VoiceConfigDialog
        config={CONFIG}
        voices={[]}
        open={false}
        onApply={() => {}}
        onCancel={() => {}}
        ttsModel={null}
        hasModelTTS={false}
      />,
    );

    const updatedConfig: VoiceConfig = { ...CONFIG, rate: 1.8 };
    rerender(
      <VoiceConfigDialog
        config={updatedConfig}
        voices={[]}
        open
        onApply={() => {}}
        onCancel={() => {}}
        ttsModel={null}
        hasModelTTS={false}
      />,
    );

    const sliders = getAllByRole('slider');
    expect(sliders[0]).toHaveAttribute('aria-valuenow', '1.8');
  });

  it('with a model configured, shows the model voices and no "no speech model" notice', () => {
    const { getByTestId, queryByTestId } = renderWithTheme(
      <VoiceConfigDialog
        config={CONFIG}
        voices={[{ id: 'v-1', name: 'Server Voice' }]}
        open
        onApply={() => {}}
        onCancel={() => {}}
        ttsModel={TTS_MODEL}
        hasModelTTS
        projectId="p1"
      />,
    );
    expect(getByTestId('voice-preview-button')).toBeInTheDocument();
    expect(queryByTestId('voice-no-model-notice')).not.toBeInTheDocument();
  });

  it('with no speech model configured, says so above the controls', () => {
    const { getByTestId } = renderWithTheme(
      <VoiceConfigDialog
        config={CONFIG}
        voices={[]}
        open
        onApply={() => {}}
        onCancel={() => {}}
        ttsModel={null}
        hasModelTTS={false}
      />,
    );
    expect(getByTestId('voice-no-model-notice')).toHaveTextContent('No speech model is configured for this project.');
  });
});
