/**
 * VoicePersonalizationSection — local port of the voice personalization panel.
 */
import { memo, useCallback, useContext, useEffect, useMemo, useState } from 'react';

import { useQuery } from '@tanstack/react-query';
import Box from '@mui/material/Box';
import Slider from '@mui/material/Slider';
import Typography from '@mui/material/Typography';

import { AccordionConstants } from '@/shared/lib/constants';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { SingleSelect } from '@/shared/ui/SingleSelect';
import { SocketClientContext } from '@/shared/api/socket/client';
import { t } from '@/shared/i18n';

import {
  VOICE_SPEED_MARKS,
  VOICE_VOLUME_MARKS,
  fetchSettingsTtsModels,
  fetchSettingsTtsVoices,
  formatPercentLabel,
  formatSpeedLabel,
  loadStored,
  pickSelectedVoice,
  shouldUseModelVoices,
  speakBrowserPreview,
  storeVoiceConfig,
  toVoiceOptions,
} from './voiceConfig.helpers';
import type { SettingsTtsVoice, VoiceConfig } from './voiceConfig.helpers';

export interface VoicePersonalizationSectionProps {
  /** Currently-selected project id — threaded down from the route. */
  projectId: string;
}

export const VoicePersonalizationSection = memo(({ projectId }: VoicePersonalizationSectionProps) => {
  const [config, setConfigState] = useState<VoiceConfig>(loadStored);
  const [browserVoices, setBrowserVoices] = useState<Array<{ name: string; localService: boolean }>>([]);

  const socket = useContext(SocketClientContext);

  const { data: ttsModels } = useQuery({
    // `section` and `include_shared` are part of the key on purpose: the
    // shared `['models', projectId]` key this panel used to share carries
    // neither. The LLM list requested elsewhere on this same page and this
    // TTS list would therefore collide on one cache entry. Whichever mounted
    // first would win.
    queryKey: ['settings', 'models', projectId, 'tts', true],
    queryFn: () => fetchSettingsTtsModels(projectId),
    enabled: !!projectId,
  });

  useEffect(() => {
    if (typeof window === 'undefined' || !('speechSynthesis' in window)) return;
    const load = () => setBrowserVoices(window.speechSynthesis.getVoices());
    load();
    window.speechSynthesis.addEventListener('voiceschanged', load);
    return () => window.speechSynthesis.removeEventListener('voiceschanged', load);
  }, []);

  const ttsModel = useMemo(
    () => ttsModels?.find((m) => m.default) ?? ttsModels?.[0] ?? null,
    [ttsModels],
  );

  // Matches the old app / the sibling `features/chat-input` port
  // (`hasModelTTS = !!(ttsModel && socket)`): model-backed TTS needs a live
  // socket connection, not just a resolved model — otherwise there is
  // nothing to actually stream audio back from.
  const hasModelTTS = !!(ttsModel && socket);

  const ttsVoicesQuery = useQuery({
    queryKey: ['settings', 'tts-voices', ttsModel?.project_id ?? projectId, ttsModel?.name],
    queryFn: () => fetchSettingsTtsVoices(ttsModel?.project_id ?? projectId, ttsModel?.name),
    enabled: hasModelTTS,
  });

  // A resolved TTS model is not enough to commit to model voices. The voice
  // route answers 501 for every project today (#466), and a TTS
  // configuration can carry no voices at all. Both cases left the Voice
  // dropdown empty AND suppressed the browser-voice fallback, because the
  // fallback was gated on `hasModelTTS` alone. Commit to model voices only
  // while the list is still loading or once it resolves non-empty.
  const modelVoices = ttsVoicesQuery.data ?? [];
  const useModelVoices = shouldUseModelVoices(
    hasModelTTS,
    ttsVoicesQuery.isSuccess || ttsVoicesQuery.isError,
    modelVoices.length,
  );

  const displayVoices: readonly (SettingsTtsVoice | { name: string; localService: boolean })[] = useModelVoices
    ? modelVoices
    : browserVoices;

  const handleConfigChange = useCallback((updates: Partial<VoiceConfig>) => {
    setConfigState((prev) => {
      const next = { ...prev, ...updates };
      storeVoiceConfig(next);
      return next;
    });
  }, []);

  const handleVoiceChange = useCallback(
    (value: string) => {
      handleConfigChange(
        useModelVoices ? { voiceId: value || null, voiceName: null } : { voiceName: value || null, voiceId: null },
      );
    },
    [handleConfigChange, useModelVoices],
  );

  const handleRateChange = useCallback(
    (_event: Event, value: number | number[]) => {
      handleConfigChange({ rate: Array.isArray(value) ? (value[0] ?? 1) : value });
    },
    [handleConfigChange],
  );
  const handleVolumeChange = useCallback(
    (_event: Event, value: number | number[]) => {
      handleConfigChange({ volume: Array.isArray(value) ? (value[0] ?? 1) : value });
    },
    [handleConfigChange],
  );

  const selectedVoiceValue = pickSelectedVoice(config, useModelVoices);

  const voiceOptions = toVoiceOptions(displayVoices, useModelVoices);

  const [isPlaying, setIsPlaying] = useState(false);
  const handlePreview = useCallback(() => {
    if (useModelVoices) {
      // Model-backed preview needs the socket + Web Audio TTS engine
      // (`features/chat-input/lib/hooks/useTextToSpeech.hooks.ts` +
      // `useModelTtsEngine.hooks.ts`). That engine is feature-private to
      // `features/chat-input`; `no-sideways-features` forbids importing it
      // from here, and duplicating its socket protocol / audio scheduling
      // is out of this fix's scope — it needs a shared-home promotion first
      // (the same path `ThemeModeToggle` took to `shared/ui` for this exact
      // page). No-op rather than silently playing the wrong (browser) voice
      // under the configured model voice's label.
      return;
    }
    speakBrowserPreview(config.rate, config.volume, setIsPlaying);
  }, [useModelVoices, config.rate, config.volume]);

  return (
    <BasicAccordion
      showMode={AccordionConstants.AccordionShowMode.LeftMode}
      slotSx={{ accordion: { background: 'transparent' } }}
      data-testid="voice-personalization-section"
      items={[
        {
          title: t('settings.voice.section', 'Voice Personalization'),
          content: (
            <Box sx={styles.content}>
              {voiceOptions.length > 0 && (
                <SingleSelect
                  label={t('settings.voice.label', 'Voice')}
                  value={selectedVoiceValue}
                  options={voiceOptions}
                  onChange={handleVoiceChange}
                  placeholder={t('settings.voice.default', 'Default')}
                />
              )}
              {/* Speed and Volume sit SIDE BY SIDE, each half the column —
                * baseline `VoiceConfigControls.jsx`'s `slidersContainer`
                * (row, 1.5rem gap, `flex: 1` rows). Stacked full-width, the
                * two sliders were twice as long as production's and pushed
                * "Preview Voice" a slider's height further down. */}
              <Box sx={styles.slidersContainer}>
                <Box sx={styles.sliderRow}>
                  <Typography variant="caption" sx={styles.sliderLabel}>
                    {t('settings.voice.speed', 'Speed')}
                  </Typography>
                  <Slider
                    value={config.rate}
                    onChange={handleRateChange}
                    min={0.5}
                    max={2}
                    step={0.1}
                    marks={VOICE_SPEED_MARKS}
                    valueLabelDisplay="auto"
                    valueLabelFormat={formatSpeedLabel}
                    size="small"
                    aria-label={t('settings.voice.speed', 'Speed')}
                    slotProps={slotProps.speedSlider}
                  />
                </Box>
                <Box sx={styles.sliderRow}>
                  <Typography variant="caption" sx={styles.sliderLabel}>
                    {t('settings.voice.volume', 'Volume')}
                  </Typography>
                  <Slider
                    value={config.volume}
                    onChange={handleVolumeChange}
                    min={0}
                    max={1}
                    step={0.05}
                    marks={VOICE_VOLUME_MARKS}
                    valueLabelDisplay="auto"
                    valueLabelFormat={formatPercentLabel}
                    size="small"
                    aria-label={t('settings.voice.volume', 'Volume')}
                    slotProps={slotProps.volumeSlider}
                  />
                </Box>
              </Box>
              {!isPlaying && (
                <Box>
                  <BaseBtn
                    variant="elitea"
                    color="secondary"
                    loading={isPlaying}
                    onClick={handlePreview}
                    data-testid="voice-preview-button"
                  >
                    {t('settings.voice.preview', 'Preview Voice')}
                  </BaseBtn>
                </Box>
              )}
            </Box>
          ),
        },
      ]}
    />
  );
});

VoicePersonalizationSection.displayName = 'VoicePersonalizationSection';

/**
 * The first/last mark labels are pulled inside the rail so they do not
 * overhang the column — baseline `VoiceConfigControls.jsx`'s
 * `speedSlider`/`volumeSlider`.
 *
 * The rule is written on the `markLabel` SLOT and qualified by the slot's own
 * `data-index` attribute. The baseline reaches down from the slider root into
 * the label's internal class, which R-T6 bans outside
 * `shared/brand/mui-overrides/`. `Slider` exposes `markLabel` as a documented
 * slot, and it renders every label with `data-index`, so the slot's own `sx`
 * can select the two end labels without naming an internal class.
 *
 * Specificity is unchanged in effect: the emitted rule is one generated class
 * plus one attribute, and the default `translateX(-50%)` it must beat is the
 * same generated class alone.
 */
const endMarkLabelSlotProps = (lastIndex: number) => ({
  markLabel: {
    sx: {
      '&[data-index="0"]': { transform: 'translateX(0)' },
      [`&[data-index="${lastIndex}"]`]: { transform: 'translateX(-100%)' },
    },
  },
});

const styles = {
  content: {
    display: 'flex',
    flexDirection: 'column',
    gap: '1rem',
  },
  slidersContainer: {
    display: 'flex',
    flexDirection: 'row',
    gap: '1.5rem',
  },
  sliderRow: {
    display: 'flex',
    flexDirection: 'column',
    gap: '0.25rem',
    px: '0.25rem',
    flex: 1,
    minWidth: 0,
  },
  sliderLabel: {
    color: 'text.default',
  },
};

/** `slotProps` for the two sliders — see {@link endMarkLabelSlotProps}. */
const slotProps = {
  speedSlider: endMarkLabelSlotProps(3),
  volumeSlider: endMarkLabelSlotProps(2),
};
