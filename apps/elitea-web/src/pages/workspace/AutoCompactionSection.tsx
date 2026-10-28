/**
 * The Context tab's "Auto-compaction" section (`useCompactionSettings`):
 * the app's switch, the share of the window that compacts, and when an idle
 * thread is summarized in the background (before the 5-minute or the
 * 1-hour prompt cache expires, after a number of minutes, or never). The
 * settings are for every folder; the choices below the switch are off with it.
 */
import { useState } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { customIdleCache, MAX_IDLE_MINUTES, MIN_IDLE_MINUTES, THRESHOLD_PERCENTS, type CompactionSettings, type IdleCompaction } from '@/shared/desktop/compactionSettings';
import { t } from '@/shared/i18n';

import { useCompactionSettings } from './useCompactionSettings';

/** The minutes a custom idle time starts with. */
const DEFAULT_CUSTOM_MINUTES = 30;
const NEAR_LIMIT = 'limit';

const muted = (theme: Theme) => ({ color: theme.vars.palette.text.metrics });

function idleLabel(idle: IdleCompaction): string {
  switch (idle) {
    case 'cache_5m':
      return t('workspace.context.auto.idle5m', 'Before the 5-minute cache expires (4 min)');
    case 'cache_1h':
      return t('workspace.context.auto.idle1h', 'Before the 1-hour cache expires (55 min) · cache writes cost 2×');
    case 'custom':
      return t('workspace.context.auto.idleCustom', 'Custom…');
    case 'off':
      return t('workspace.context.auto.idleOff', 'Off');
  }
}

const IDLE_CHOICES: readonly IdleCompaction[] = ['cache_5m', 'cache_1h', 'custom', 'off'];

/** A whole number of minutes in range, else null. */
function parseMinutes(text: string): number | null {
  const minutes = /^\d+$/u.test(text.trim()) ? Number(text.trim()) : Number.NaN;
  return Number.isInteger(minutes) && minutes >= MIN_IDLE_MINUTES && minutes <= MAX_IDLE_MINUTES ? minutes : null;
}

/** What a custom number of minutes costs, under its field. */
function minutesNote(minutes: number | null): { text: string; warn: boolean } {
  if (minutes === null) return { text: t('workspace.context.auto.minutesRange', 'From 1 to 600 minutes.'), warn: true };
  const cache = customIdleCache(minutes);
  if (cache === 'past') return { text: t('workspace.context.auto.pastCache', 'Later than the cache lasts: idle compaction won’t run'), warn: true };
  if (cache === 'cache_1h') return { text: t('workspace.context.auto.hourCache', 'Uses the 1-hour cache: cache writes cost 2×'), warn: false };
  return { text: '', warn: false };
}

interface MinutesProps {
  minutes: number;
  disabled: boolean;
  onSave: (minutes: number) => void;
}

/** The custom idle time: saved on Enter or when the field loses focus, if it is a valid number. */
function CustomMinutes({ minutes, disabled, onSave }: MinutesProps): React.JSX.Element {
  const [text, setText] = useState(String(minutes));
  // Changed elsewhere (another window): the field follows.
  const [shown, setShown] = useState(minutes);
  if (shown !== minutes) {
    setShown(minutes);
    setText(String(minutes));
  }
  const parsed = parseMinutes(text);
  const note = minutesNote(parsed);
  const commit = (): void => {
    if (parsed !== null && parsed !== minutes) onSave(parsed);
  };
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <TextField
        size="small"
        type="number"
        label={t('workspace.context.auto.minutes', 'Minutes idle')}
        value={text}
        disabled={disabled}
        error={parsed === null}
        onChange={(event) => setText(event.target.value)}
        onBlur={commit}
        onKeyDown={(event) => event.key === 'Enter' && commit()}
        slotProps={{ htmlInput: { min: MIN_IDLE_MINUTES, max: MAX_IDLE_MINUTES, 'data-testid': 'auto-compaction-minutes' } }}
      />
      {note.text !== '' && (
        <Typography data-testid="auto-compaction-minutes-note" variant="bodySmall" sx={(theme: Theme) => (note.warn ? { color: theme.vars.palette.warning.main } : muted(theme))}>
          {note.text}
        </Typography>
      )}
    </Box>
  );
}

function Choices({ settings, save }: { settings: CompactionSettings; save: (next: CompactionSettings) => void }): React.JSX.Element {
  const off = !settings.auto;
  const threshold = settings.threshold_percent === null ? NEAR_LIMIT : String(settings.threshold_percent);
  const pickIdle = (idle: IdleCompaction): void =>
    save({ ...settings, idle, idle_minutes: idle === 'custom' ? (settings.idle_minutes ?? DEFAULT_CUSTOM_MINUTES) : null });
  return (
    <>
      <TextField
        select
        size="small"
        label={t('workspace.context.auto.threshold', 'Compact when context reaches')}
        value={threshold}
        disabled={off}
        helperText={t('workspace.context.auto.thresholdHint', 'Lower saves on every request but forgets detail sooner.')}
        onChange={(event) => save({ ...settings, threshold_percent: event.target.value === NEAR_LIMIT ? null : Number(event.target.value) })}
        slotProps={{ htmlInput: { 'data-testid': 'auto-compaction-threshold' } }}
      >
        <MenuItem value={NEAR_LIMIT}>{t('workspace.context.auto.nearLimit', 'Near the limit')}</MenuItem>
        {THRESHOLD_PERCENTS.map((percent) => (
          <MenuItem key={percent} value={String(percent)}>{`${String(percent)}%`}</MenuItem>
        ))}
      </TextField>
      <TextField
        select
        size="small"
        label={t('workspace.context.auto.idle', 'When idle')}
        value={settings.idle}
        disabled={off}
        onChange={(event) => pickIdle(event.target.value as IdleCompaction)}
        slotProps={{ htmlInput: { 'data-testid': 'auto-compaction-idle' } }}
      >
        {IDLE_CHOICES.map((idle) => (
          <MenuItem key={idle} value={idle}>
            {idleLabel(idle)}
          </MenuItem>
        ))}
      </TextField>
      {settings.idle === 'custom' && (
        <CustomMinutes minutes={settings.idle_minutes ?? DEFAULT_CUSTOM_MINUTES} disabled={off} onSave={(minutes) => save({ ...settings, idle_minutes: minutes })} />
      )}
    </>
  );
}

export function AutoCompactionSection(): React.JSX.Element | null {
  const { settings, available, error, save } = useCompactionSettings();
  if (!available) return null;
  return (
    <Box data-testid="auto-compaction" sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.25 }}>
        <Typography variant="headingSmall">{t('workspace.context.auto.title', 'Auto-compaction')}</Typography>
        <Typography variant="bodySmall" sx={muted}>
          {t('workspace.context.auto.scope', 'For all folders.')}
        </Typography>
      </Box>
      {settings !== undefined && (
        <>
          <FormControlLabel
            control={<Switch size="small" checked={settings.auto} onChange={(_event, auto) => save({ ...settings, auto })} />}
            label={<Typography variant="bodySmall">{t('workspace.context.auto.switch', 'Compact automatically')}</Typography>}
          />
          <Choices settings={settings} save={save} />
        </>
      )}
      {error !== null && (
        <Typography role="alert" variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.error.main })}>
          {error}
        </Typography>
      )}
      <Typography variant="bodySmall" sx={muted}>
        {t('workspace.context.auto.help', 'Only threads with 60k+ tokens of conversation on paid models with a prompt cache. Skipped while you type or after the computer sleeps.')}
      </Typography>
    </Box>
  );
}
