import type { ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import Box from '@mui/material/Box';
import Slider from '@mui/material/Slider';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import type { SocketClient } from '@/shared/api/socket/client';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { SingleSelect } from '@/shared/ui/SingleSelect';
import type { SingleSelectOption } from '@/shared/ui/SingleSelectMenuItem';

import type { TtsVoice } from '../api/ttsVoices';
import { VOICE_PREVIEW_TEXT, VOICE_SPEED_MARKS, VOICE_VOLUME_MARKS } from '../lib/constants/voice.constants';
import { useTextToSpeech } from '../lib/hooks/useTextToSpeech.hooks';
import type { TtsModel } from '../lib/hooks/useTextToSpeech.types';
import type { VoiceConfig, VoiceConfigUpdate } from '../lib/hooks/useVoiceConfig.hooks';

/**
 * Ported from
 * apps/elitea-ui/src/[fsd]/features/chat/voice-config/ui/VoiceConfigControls.jsx.
 *
 * There is no `MuiSlider` entry in `shared/brand/mui-overrides/` (same gap
 * `shared/ui/DiscreteSlider.tsx`'s own doc comment documents), so this
 * renders MUI's `Slider` directly with no slot-level styling — R-T6 confines
 * `.MuiSlider-*` selectors to that directory. `DiscreteSlider` itself is not
 * reused here: its integer-only `step={1}` marks don't fit the baseline's
 * 0.1/0.05-step decimal speed/volume ranges (see this unit's final report).
 */
export interface VoiceConfigControlsProps {
  readonly config: VoiceConfig;
  readonly onConfigChange: (updates: VoiceConfigUpdate) => void;
  readonly hasModelTTS: boolean;
  readonly ttsModel: TtsModel | null;
  readonly socket: SocketClient | null;
  readonly browserVoices: readonly SpeechSynthesisVoice[];
  readonly voices: readonly (TtsVoice | SpeechSynthesisVoice)[];
  readonly isPlaying?: boolean | undefined;
}

function toVoiceOption(voice: TtsVoice | SpeechSynthesisVoice, hasModelTTS: boolean): SingleSelectOption {
  if (hasModelTTS) {
    const serverVoice = voice as TtsVoice;
    return { value: serverVoice.id ?? serverVoice.name, label: serverVoice.name };
  }
  const browserVoice = voice as SpeechSynthesisVoice;
  return { value: browserVoice.name, label: browserVoice.localService ? browserVoice.name : `${browserVoice.name} ${t('features.chatInput.voiceConfigControls.online', '(online)')}` };
}

export function VoiceConfigControls(props: VoiceConfigControlsProps): ReactNode {
  const { config, onConfigChange, hasModelTTS, ttsModel, socket, browserVoices, voices, isPlaying } = props;

  const previewVoiceConfig = useMemo(
    () => ({
      voice: browserVoices.find((voice) => voice.name === config.voiceName) ?? browserVoices[0] ?? null,
      voiceId: config.voiceId || undefined,
      rate: config.rate,
      volume: config.volume,
    }),
    [config, browserVoices],
  );

  const { speak: previewSpeak, isPlaying: isPreviewPlaying } = useTextToSpeech({
    ttsModel: hasModelTTS ? ttsModel : null,
    socket,
    voiceConfig: previewVoiceConfig,
  });

  const voiceOptions = useMemo(() => voices.map((voice) => toVoiceOption(voice, hasModelTTS)), [voices, hasModelTTS]);
  const selectedVoiceValue = hasModelTTS ? (config.voiceId ?? '') : (config.voiceName ?? '');

  const handleVoiceChange = useCallback(
    (value: string) => {
      if (hasModelTTS) onConfigChange({ voiceId: value || null, voiceName: null });
      else onConfigChange({ voiceName: value || null, voiceId: null });
    },
    [hasModelTTS, onConfigChange],
  );

  const handleRateChange = useCallback(
    (_event: Event, value: number | number[]) => {
      if (typeof value === 'number') onConfigChange({ rate: value });
    },
    [onConfigChange],
  );

  const handleVolumeChange = useCallback(
    (_event: Event, value: number | number[]) => {
      if (typeof value === 'number') onConfigChange({ volume: value });
    },
    [onConfigChange],
  );

  const handlePreview = useCallback(() => previewSpeak(VOICE_PREVIEW_TEXT), [previewSpeak]);

  return (
    <Box sx={contentSx}>
      {voiceOptions.length > 0 && (
        <SingleSelect
          label={t('features.chatInput.voiceConfigControls.voiceLabel', 'Voice')}
          value={selectedVoiceValue}
          options={voiceOptions}
          onChange={handleVoiceChange}
          placeholder={t('features.chatInput.voiceConfigControls.defaultPlaceholder', 'Default')}
        />
      )}
      <Box sx={sliderRowSx}>
        <Typography
          variant="bodySmall"
          sx={sliderLabelSx}
        >
          {t('features.chatInput.voiceConfigControls.speedLabel', 'Speed')}
        </Typography>
        <Slider
          value={config.rate}
          min={0.5}
          max={2.0}
          step={0.1}
          marks={VOICE_SPEED_MARKS}
          onChange={handleRateChange}
          valueLabelDisplay="auto"
          valueLabelFormat={(value) => `${value}×`}
          size="small"
          aria-label={t('features.chatInput.voiceConfigControls.speedLabel', 'Speed')}
        />
      </Box>
      <Box sx={sliderRowSx}>
        <Typography
          variant="bodySmall"
          sx={sliderLabelSx}
        >
          {t('features.chatInput.voiceConfigControls.volumeLabel', 'Volume')}
        </Typography>
        <Slider
          value={config.volume}
          min={0}
          max={1}
          step={0.05}
          marks={VOICE_VOLUME_MARKS}
          onChange={handleVolumeChange}
          valueLabelDisplay="auto"
          valueLabelFormat={(value) => `${Math.round(value * 100)}%`}
          size="small"
          aria-label={t('features.chatInput.voiceConfigControls.volumeLabel', 'Volume')}
        />
      </Box>
      {!isPlaying && (
        <Box>
          <BaseBtn
            variant="secondary"
            loading={isPreviewPlaying}
            onClick={handlePreview}
            data-testid="voice-preview-button"
          >
            {t('features.chatInput.voiceConfigControls.previewVoice', 'Preview Voice')}
          </BaseBtn>
        </Box>
      )}
    </Box>
  );
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem' };
const sliderRowSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.25rem', px: '0.25rem' };
const sliderLabelSx: SxProps<Theme> = { color: 'text.secondary' };
