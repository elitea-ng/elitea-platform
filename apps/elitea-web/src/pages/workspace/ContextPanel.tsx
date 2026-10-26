/**
 * The Context tab of a local thread's inspector (#1217): how full the
 * model's window was at the thread's latest model call, and what fills it.
 * A bar and one row per part (system prompt, tools, MCP tools, skills,
 * messages), then the compaction's buffer and the free space, each in
 * tokens and as a share of the window (`contextBreakdown`). The figures are
 * the host's estimates of the request it sent, never its content (IPC.md
 * "Context window"). While a summary call runs the tab says so. The
 * switch below turns idle compaction off for the app (`useIdleCompaction`).
 * Loaded lazily by `SessionPanel`.
 */
import { useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { ContextMeter } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import { contextBreakdown, formatPercent, formatTokens, updatedAgo, WARN_PERCENT, type ContextRowKey } from './contextBreakdown';
import { setIdleCompaction, useIdleCompactionEnabled } from './useIdleCompaction';

const ROW_COLOR: Readonly<Record<ContextRowKey, (theme: Theme) => string>> = {
  system: (theme) => theme.vars.palette.primary.main,
  tools: (theme) => theme.vars.palette.secondary.main,
  mcp: (theme) => theme.vars.palette.success.main,
  skills: (theme) => theme.vars.palette.info.main,
  messages: (theme) => theme.vars.palette.warning.main,
  used: (theme) => theme.vars.palette.primary.main,
  buffer: (theme) => theme.vars.palette.text.disabled,
  free: (theme) => theme.vars.palette.border.lines,
};

/** The time now, again each minute (for "Updated 2 min ago"). */
function useMinuteClock(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 60_000);
    return () => window.clearInterval(id);
  }, []);
  return now;
}

function Notes({ meter }: { meter: ContextMeter }): React.JSX.Element {
  const now = useMinuteClock();
  const notes = [updatedAgo(meter.measured_at, now)];
  if (meter.compacted_at !== undefined) {
    notes.push(t('workspace.context.compactedAt', 'Summarized at {{time}}', { time: new Date(meter.compacted_at).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }) }));
  }
  if (meter.window_fallback) notes.push(t('workspace.context.fallback', 'The model’s window is unknown: 128K is assumed.'));
  if (meter.parts === undefined) notes.push(t('workspace.context.noParts', 'This app version does not report the parts of the context.'));
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      {notes
        .filter((note): note is string => note !== null)
        .map((note) => (
          <Typography key={note} variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
            {note}
          </Typography>
        ))}
    </Box>
  );
}

/** The app's switch for `useIdleCompaction`. */
function IdleSwitch(): React.JSX.Element {
  const enabled = useIdleCompactionEnabled();
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.25 }}>
      <FormControlLabel
        control={<Switch size="small" checked={enabled} onChange={(_event, checked) => setIdleCompaction(checked)} />}
        label={<Typography variant="bodySmall">{t('workspace.context.idleSwitch', 'Summarize after the thread is idle')}</Typography>}
      />
      <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {t('workspace.context.idleHelp', 'After 5 minutes without a turn, the next message first summarizes a long conversation. This is for all folders.')}
      </Typography>
    </Box>
  );
}

export default function ContextPanel({ meter }: { meter: ContextMeter | undefined }): React.JSX.Element {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Figures meter={meter} />
      <IdleSwitch />
    </Box>
  );
}

function Figures({ meter }: { meter: ContextMeter | undefined }): React.JSX.Element {
  if (meter === undefined) {
    return (
      <Typography data-testid="context-panel-empty" variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}>
        {t('workspace.context.empty', 'Send a message to see what fills the model’s context.')}
      </Typography>
    );
  }
  const breakdown = contextBreakdown(meter);
  const warn = breakdown.percent >= WARN_PERCENT;
  const summary =
    meter.phase === 'compacting' ? t('workspace.context.compacting', 'Summarizing the conversation…') : t('workspace.context.percentUsed', '{{percent}}% used', { percent: breakdown.percent });
  return (
    <Box data-testid="context-panel" data-percent={breakdown.percent} sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
      <Box sx={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between', gap: 1 }}>
        <Typography variant="headingSmall" sx={(theme: Theme) => ({ color: warn ? theme.vars.palette.warning.main : theme.vars.palette.text.secondary })}>
          {summary}
        </Typography>
        <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, fontVariantNumeric: 'tabular-nums' })}>
          {t('workspace.context.ofWindow', '{{used}} / {{window}}', { used: formatTokens(breakdown.usedTokens), window: formatTokens(breakdown.windowTokens) })}
        </Typography>
      </Box>
      <Box aria-hidden sx={{ display: 'flex', height: 8, borderRadius: 1, overflow: 'hidden' }}>
        {breakdown.rows.map((row) => (
          <Box key={row.key} sx={(theme: Theme) => ({ width: `${String(row.percent)}%`, backgroundColor: ROW_COLOR[row.key](theme) })} />
        ))}
      </Box>
      <Box component="ul" sx={{ listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 0.75 }}>
        {breakdown.rows.map((row) => (
          <Box component="li" key={row.key} data-testid={`context-row-${row.key}`} sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
            <Box aria-hidden sx={(theme: Theme) => ({ width: 10, height: 10, borderRadius: 0.5, flexShrink: 0, backgroundColor: ROW_COLOR[row.key](theme) })} />
            <Typography variant="bodySmall" sx={{ flex: 1, minWidth: 0 }}>
              {row.label}
            </Typography>
            <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, fontVariantNumeric: 'tabular-nums', minWidth: 48, textAlign: 'right' })}>
              {formatTokens(row.tokens)}
            </Typography>
            <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, fontVariantNumeric: 'tabular-nums', minWidth: 48, textAlign: 'right' })}>
              {formatPercent(row.percent)}
            </Typography>
          </Box>
        ))}
      </Box>
      <Notes meter={meter} />
    </Box>
  );
}
