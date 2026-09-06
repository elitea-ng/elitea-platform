/**
 * SoundNotificationControls — volume slider + toggle for sound notifications.
 */
import { memo, useCallback } from 'react';

import Box from '@mui/material/Box';
import Slider from '@mui/material/Slider';
import Typography from '@mui/material/Typography';

import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseSwitch } from '@/shared/ui/BaseSwitch';

import { type UseSoundNotificationResult } from '@/shared/lib/hooks/useSoundNotification';
import { t } from '@/shared/i18n';

const VOLUME_MARKS = [
  { value: 0, label: '0%' },
  { value: 0.5, label: '50%' },
  { value: 1, label: '100%' },
];

export interface SoundNotificationControlsProps {
  config: UseSoundNotificationResult['config'];
  setConfig: UseSoundNotificationResult['setConfig'];
  playCompletionSound: () => void;
}

export const SoundNotificationControls = memo(
  ({ config, setConfig, playCompletionSound }: SoundNotificationControlsProps) => {
    const handleToggle = useCallback(
      (_event: React.ChangeEvent<HTMLInputElement>, checkedValue: boolean) => {
        setConfig({ enabled: checkedValue });
      },
      [setConfig],
    );

    const handleVolumeChange = useCallback(
      (_event: Event, value: number | number[]) => {
        const normalized = Array.isArray(value) ? (value[0] ?? 0) : value;
        setConfig({ volume: Math.max(0, Math.min(1, normalized)) });
      },
      [setConfig],
    );

    return (
      <Box sx={styles.content}>
        {/* A CARD, not a checkbox row. The baseline
          * (`sound-notification/SoundNotificationControls.jsx`) renders a
          * filled 0.75rem-radius panel holding a heading, a one-line
          * description and the switch pushed to the far right — the same
          * `SettingsToggleCard` shape Settings › Memory already uses three
          * times. This was a bare `FormControlLabel`: switch first, label
          * beside it, no surface, no description, nothing aligned with the
          * cards on the sibling tabs. */}
        <Box sx={styles.toggleSection}>
          <Box sx={styles.toggleContent}>
            <Typography variant="headingSmall" sx={styles.toggleTitle}>
              {t('settings.profile.soundNotifications.title', 'Sound Notifications')}
            </Typography>
            <Typography variant="bodySmall">
              {t('settings.playSoundOnComplete', 'Play sound when tasks complete')}
            </Typography>
          </Box>
          <BaseSwitch
            checked={config.enabled}
            onChange={handleToggle}
            slotProps={{
              input: {
                'aria-label': t('settings.playSoundOnComplete', 'Play sound when tasks complete'),
              },
            }}
          />
        </Box>
        {config.enabled && (
          <Box sx={styles.sliderRow}>
            <Typography variant="caption" sx={styles.sliderLabel}>
              {t('settings.volume', 'Volume')}
            </Typography>
            <Slider
              value={config.volume}
              onChange={handleVolumeChange}
              min={0}
              max={1}
              step={0.05}
              marks={VOLUME_MARKS}
              valueLabelDisplay="auto"
              valueLabelFormat={formatPercentLabel}
              size="small"
              aria-label={t('settings.volume', 'Volume')}
              sx={styles.slider}
            />
          </Box>
        )}
        {config.enabled && (
          <Box>
            <BaseBtn variant="elitea" color="secondary" onClick={playCompletionSound}>
              {t('settings.previewSound', 'Preview Sound')}
            </BaseBtn>
          </Box>
        )}
      </Box>
    );
  },
);

SoundNotificationControls.displayName = 'SoundNotificationControls';

/** `0.5` -> `50%` in the drag bubble — baseline `valueLabelFormat`. */
function formatPercentLabel(value: number): string {
  return `${Math.round(value * 100)}%`;
}

const styles = {
  content: {
    display: 'flex',
    flexDirection: 'column',
    gap: '1rem',
  },
  toggleSection: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    padding: '0.75rem 1rem',
    backgroundColor: 'background.userInputBackground',
    // oxlint-disable-next-line elitea/ad-hoc-radius -- baseline literal; see SettingsToggleCard for why no token fits.
    borderRadius: '0.75rem',
  },
  toggleContent: {
    display: 'flex',
    flexDirection: 'column',
    gap: '0.25rem',
  },
  toggleTitle: {
    color: 'text.secondary',
  },
  /** Half the column, as on Memory's paired numeric fields — baseline `sliderRow`. */
  sliderRow: {
    display: 'flex',
    flexDirection: 'column',
    gap: '0.25rem',
    px: '0.25rem',
    width: '48%',
  },
  sliderLabel: {
    color: 'text.secondary',
  },
  slider: {
    // oxlint-disable-next-line elitea/no-mui-internal-selector -- ported verbatim from the baseline's own sx; MUI exposes no per-mark slotProps to express this.
    '& .MuiSlider-markLabel[data-index="0"]': { transform: 'translateX(0)' },
    // oxlint-disable-next-line elitea/no-mui-internal-selector -- see above.
    '& .MuiSlider-markLabel[data-index="2"]': { transform: 'translateX(-100%)' },
  },
};
