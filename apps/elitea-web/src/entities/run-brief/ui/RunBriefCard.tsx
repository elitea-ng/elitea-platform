/**
 * The run brief at the chain's first answer (ADR-0045 decision 11). Collapsed
 * it is one line, "Goal · Done when: 3 checks"; its control opens the goal,
 * the conditions, the constraints and the assumptions. A small model wrote
 * the brief from the person's request, so every field shows as plain text,
 * never as Markdown or HTML.
 *
 * `pending` shows a small "Writing the brief…" line; `none`, `off` and
 * `failed` show nothing. A `ready` brief with no conditions has no check: the
 * card is its goal line only.
 */
import { useId, useState } from 'react';

import ExpandLessIcon from '@mui/icons-material/ExpandLess';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import FlagOutlinedIcon from '@mui/icons-material/FlagOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { RunBrief } from '@/shared/desktop/unattendedIpc';
import { t } from '@/shared/i18n';

import type { RunBriefView } from '../model/runBrief';

const muted = (theme: Theme) => ({ color: theme.vars.palette.text.metrics });

const cardSx = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: 0.5,
  alignSelf: 'stretch',
  paddingX: 1,
  paddingY: 0.5,
  borderRadius: theme.vars.shape.radiusSm,
  border: `0.0625rem solid ${theme.vars.palette.divider}`,
});

function checksText(count: number): string {
  return count === 1 ? t('unattended.brief.checksOne', 'Done when: 1 check') : t('unattended.brief.checks', 'Done when: {{n}} checks', { n: String(count) });
}

function Section({ title, items }: { title: string; items: readonly string[] }): React.JSX.Element | null {
  if (items.length === 0) return null;
  return (
    <Box>
      <Typography variant="labelSmall" component="h3" sx={muted}>
        {title}
      </Typography>
      <Box component="ul" sx={{ margin: 0, paddingLeft: 2.5 }}>
        {items.map((item, index) => (
          <Typography key={index} variant="bodySmall" component="li" sx={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>
            {item}
          </Typography>
        ))}
      </Box>
    </Box>
  );
}

function ReadyBrief({ brief }: { brief: RunBrief }): React.JSX.Element {
  const [open, setOpen] = useState(false);
  const detailsId = useId();
  const checks = brief.done_when.length;
  const label = t('unattended.brief.goal', 'Goal');
  return (
    <Box data-testid="run-brief-card" data-state="ready" sx={cardSx}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.75, minWidth: 0 }}>
        <FlagOutlinedIcon aria-hidden sx={(theme: Theme) => ({ ...muted(theme), width: '1rem', height: '1rem', flexShrink: 0 })} />
        <Typography variant="bodySmall" noWrap={!open} sx={{ flex: 1, minWidth: 0 }} title={open ? undefined : brief.goal}>
          <Box component="span" sx={{ fontWeight: 600 }}>
            {label}
          </Box>
          {open ? '' : `: ${brief.goal}`}
          {checks > 0 && (
            <Box component="span" sx={muted}>
              {` · ${checksText(checks)}`}
            </Box>
          )}
        </Typography>
        {checks > 0 && (
          <IconButton
            size="small"
            aria-expanded={open}
            aria-controls={detailsId}
            aria-label={open ? t('unattended.brief.hide', 'Hide the brief') : t('unattended.brief.show', 'Show the brief')}
            onClick={() => setOpen(!open)}
            data-testid="run-brief-card-toggle"
            sx={{ padding: 0.25 }}
          >
            {open ? <ExpandLessIcon fontSize="small" /> : <ExpandMoreIcon fontSize="small" />}
          </IconButton>
        )}
      </Box>
      {open && (
        <Box id={detailsId} data-testid="run-brief-card-details" sx={{ display: 'flex', flexDirection: 'column', gap: 0.75, paddingLeft: 2.75, paddingBottom: 0.5 }}>
          <Typography variant="bodySmall" sx={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>
            {brief.goal}
          </Typography>
          <Section title={t('unattended.brief.doneWhen', 'Done when')} items={brief.done_when.map((condition) => condition.text)} />
          <Section title={t('unattended.brief.constraints', 'Constraints')} items={brief.constraints} />
          <Section title={t('unattended.brief.assumptions', 'Assumptions')} items={brief.assumptions} />
          <Typography variant="bodySmall" sx={muted}>
            {t('unattended.brief.note', 'A model wrote this brief from your request. Your request wins when they disagree.')}
          </Typography>
        </Box>
      )}
    </Box>
  );
}

export function RunBriefCard({ value }: { value: RunBriefView | undefined }): React.JSX.Element | null {
  if (value?.state === 'pending') {
    return (
      <Typography variant="bodySmall" data-testid="run-brief-card" data-state="pending" sx={(theme: Theme) => ({ ...muted(theme), display: 'block' })}>
        {t('unattended.brief.pending', 'Writing the brief…')}
      </Typography>
    );
  }
  return value?.state === 'ready' && value.brief !== undefined ? <ReadyBrief brief={value.brief} /> : null;
}
