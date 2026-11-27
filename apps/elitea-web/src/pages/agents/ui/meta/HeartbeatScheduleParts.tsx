/**
 * Two composite fields of the heartbeat schedule (issue 3137): the quiet
 * hours, held on the wire as one `"HH:MM-HH:MM Zone"` string and edited as a
 * start, an end and an IANA zone; and the watch list, the kinds a wake reads
 * (with the read-only tool `toolkit_read` names).
 */
import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Autocomplete from '@mui/material/Autocomplete';
import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import FormGroup from '@mui/material/FormGroup';
import FormHelperText from '@mui/material/FormHelperText';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import type { MetaBlockHandle } from './metaControls';
import { metaFieldsetSx, metaFieldsSx } from './metaSection.styles';

interface PartProps {
  readonly handle: MetaBlockHandle;
  readonly disabled?: boolean | undefined;
}

interface QuietHours {
  readonly start: string;
  readonly end: string;
  readonly zone: string;
}

const QUIET = /^(\S*)-(\S*)(?: (.*))?$/;

function readQuietHours(value: unknown): QuietHours {
  const match = typeof value === 'string' ? QUIET.exec(value) : null;
  return { start: match?.[1] ?? '', end: match?.[2] ?? '', zone: match?.[3] ?? '' };
}

/** The wire string, or `undefined` when all three are empty (no quiet hours). */
function writeQuietHours({ start, end, zone }: QuietHours): string | undefined {
  return start === '' && end === '' && zone === '' ? undefined : `${start}-${end} ${zone}`.trimEnd();
}

function timeZones(): readonly string[] {
  try {
    return Intl.supportedValuesOf('timeZone');
  } catch {
    // Handled (§3.6): an engine without the list still takes a typed zone.
    return [];
  }
}

export function HeartbeatQuietHours({ handle, disabled }: PartProps): ReactNode {
  const quiet = readQuietHours(handle.get(['quiet_hours']));
  const zones = useMemo(() => timeZones(), []);
  const problem = handle.problem(['quiet_hours']);
  const update = (patch: Partial<QuietHours>): void => handle.set(['quiet_hours'], writeQuietHours({ ...quiet, ...patch }));
  return (
    <Box component="fieldset" sx={metaFieldsetSx}>
      <Typography component="legend" variant="labelMedium">
        {t('pages.agents.editApplication.meta.heartbeat.quietHours', 'Quiet hours')}
      </Typography>
      <Box sx={metaFieldsSx}>
        <TextField
          type="time"
          label={t('pages.agents.editApplication.meta.heartbeat.quietFrom', 'From')}
          value={quiet.start}
          onChange={(event) => update({ start: event.target.value })}
          disabled={disabled}
          size="small"
          slotProps={{ inputLabel: { shrink: true }, htmlInput: { 'data-testid': 'agent-meta-heartbeat-quiet-start' } }}
        />
        <TextField
          type="time"
          label={t('pages.agents.editApplication.meta.heartbeat.quietTo', 'To')}
          value={quiet.end}
          onChange={(event) => update({ end: event.target.value })}
          disabled={disabled}
          size="small"
          slotProps={{ inputLabel: { shrink: true }, htmlInput: { 'data-testid': 'agent-meta-heartbeat-quiet-end' } }}
        />
        <Autocomplete
          freeSolo
          options={zones}
          value={quiet.zone}
          onInputChange={(_event, value) => update({ zone: value })}
          disabled={disabled}
          size="small"
          renderInput={(params) => (
            <TextField
              {...params}
              label={t('pages.agents.editApplication.meta.heartbeat.zone', 'Time zone')}
              slotProps={{ ...params.slotProps, htmlInput: { ...params.slotProps.htmlInput, 'data-testid': 'agent-meta-heartbeat-quiet-zone' } }}
            />
          )}
        />
      </Box>
      <FormHelperText error={problem !== undefined}>
        {problem ?? t('pages.agents.editApplication.meta.heartbeat.quietHelper', 'No wake starts inside the window. Leave all three empty for none.')}
      </FormHelperText>
    </Box>
  );
}

const WATCH_KINDS = ['conversation', 'schedule_note', 'toolkit_read'] as const;
type WatchItem = Readonly<{ kind: string; tool?: string }>;

function watchLabel(kind: (typeof WATCH_KINDS)[number]): string {
  if (kind === 'conversation') return t('pages.agents.editApplication.meta.heartbeat.watchConversation', 'Its conversations');
  if (kind === 'schedule_note') return t('pages.agents.editApplication.meta.heartbeat.watchScheduleNote', 'Its schedule notes');
  return t('pages.agents.editApplication.meta.heartbeat.watchToolkitRead', 'A read-only tool');
}

export function HeartbeatWatchList({ handle, disabled }: PartProps): ReactNode {
  const raw = handle.get(['watch']);
  const watch: WatchItem[] = Array.isArray(raw) ? (raw as WatchItem[]) : [];
  const toolItem = watch.find((item) => item.kind === 'toolkit_read');
  const set = (next: WatchItem[]): void => handle.set(['watch'], next.length === 0 ? undefined : next);
  const toggle = (kind: string, checked: boolean): void =>
    set(checked ? [...watch, kind === 'toolkit_read' ? { kind, tool: '' } : { kind }] : watch.filter((item) => item.kind !== kind));
  const problem = handle.problem(['watch']);
  return (
    <Box component="fieldset" sx={metaFieldsetSx}>
      <Typography component="legend" variant="labelMedium">
        {t('pages.agents.editApplication.meta.heartbeat.watch', 'A wake reads')}
      </Typography>
      <FormGroup row>
        {WATCH_KINDS.map((kind) => (
          <FormControlLabel
            key={kind}
            label={watchLabel(kind)}
            disabled={disabled}
            control={<BaseCheckbox checked={watch.some((item) => item.kind === kind)} onChange={(_event, checked) => toggle(kind, checked)} slotProps={{ input: { 'data-testid': `agent-meta-heartbeat-watch-${kind.replace('_', '-')}` } as Record<string, string> }} />}
          />
        ))}
      </FormGroup>
      {toolItem !== undefined && (
        <TextField
          label={t('pages.agents.editApplication.meta.heartbeat.watchTool', 'Tool (toolkit/tool)')}
          value={toolItem.tool ?? ''}
          onChange={(event) => set(watch.map((item) => (item === toolItem ? { kind: 'toolkit_read', tool: event.target.value } : item)))}
          error={problem !== undefined}
          disabled={disabled}
          size="small"
          slotProps={{ htmlInput: { maxLength: 128, 'data-testid': 'agent-meta-heartbeat-watch-tool' } }}
        />
      )}
      <FormHelperText error={problem !== undefined}>
        {problem ?? t('pages.agents.editApplication.meta.heartbeat.watchHelper', 'None ticked reads the default: conversations and schedule notes.')}
      </FormHelperText>
    </Box>
  );
}
