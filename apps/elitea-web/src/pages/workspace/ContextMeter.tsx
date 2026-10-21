/**
 * The composer's context meter (#1217): how full the model's window was at
 * the turn's latest model call, as a small ring and "62% of 200K"; the
 * tooltip names the tokens and the last compaction. The numbers are the
 * host's estimates of the request it sent, never its content (IPC.md
 * "Context window"). Loaded lazily by `ThreadComposer`.
 *
 * The activity island keeps its own token counts per turn; one context
 * display, here, is the meter (#1227 folds the island's into it).
 */
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { ContextMeter as Meter } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

/** At or past this share of the window the ring turns to a warning. */
const WARN_PERCENT = 80;

const compactNumber = (value: number): string => new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 0 }).format(value);

export interface MeterText {
  percent: number;
  label: string;
  detail: string;
}

/** What the meter says about `meter`. */
export function describeMeter(meter: Meter): MeterText {
  const percent = meter.window_tokens > 0 ? Math.min(100, Math.round((meter.used_tokens / meter.window_tokens) * 100)) : 0;
  const window = compactNumber(meter.window_tokens);
  const label = t('workspace.context.label', '{{percent}}% of {{window}}', { percent, window });
  const lines = [t('workspace.context.used', '{{used}} of {{window}} tokens of context in use', { used: compactNumber(meter.used_tokens), window })];
  if (meter.phase === 'compacting') lines.push(t('workspace.context.compacting', 'Summarizing the conversation…'));
  else if (meter.compacted_at !== undefined) {
    lines.push(t('workspace.context.compactedAt', 'Summarized at {{time}}', { time: new Date(meter.compacted_at).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }) }));
  }
  if (meter.window_fallback) lines.push(t('workspace.context.fallback', 'The model’s window is unknown: 32K is assumed.'));
  return { percent, label, detail: lines.join(' · ') };
}

export default function ContextMeter({ meter }: { meter: Meter }): React.JSX.Element {
  const { percent, label, detail } = describeMeter(meter);
  return (
    <Tooltip title={detail}>
      <Box
        component="span"
        data-testid="context-meter"
        data-percent={percent}
        aria-label={t('workspace.context.aria', 'Context in use: {{label}}', { label })}
        sx={{ display: 'inline-flex', alignItems: 'center', gap: 0.5, color: 'text.secondary', flexShrink: 0 }}
      >
        <CircularProgress variant="determinate" value={percent} size={14} thickness={6} color={percent >= WARN_PERCENT ? 'warning' : 'primary'} aria-hidden />
        <Typography component="span" variant="labelSmall" aria-hidden>
          {label}
        </Typography>
      </Box>
    </Tooltip>
  );
}
