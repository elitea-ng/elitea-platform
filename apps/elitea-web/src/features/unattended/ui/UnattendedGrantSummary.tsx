/**
 * The grant, read only (ADR-0045: the person sees every part before the run
 * starts): what it answers, what it does not answer (a question it does not
 * answer takes its default at once, with no card), and the chain
 * limits.
 */
import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { UnattendedGrantValue } from '../model/grant';
import { CHAIN_PARTS, sensitiveLabel, visibleParts } from './grantParts';

export interface UnattendedGrantSummaryProps {
  value: UnattendedGrantValue;
  /** Lists the desktop-only parts (default true). */
  desktop?: boolean;
  /** Lists the cloud-only parts: the output limit and the sensitive tools (default false). */
  cloud?: boolean;
}

const muted = (theme: Theme) => ({ color: theme.vars.palette.text.metrics });

function Line({ title, items, testId }: { title: string; items: string[]; testId: string }): React.JSX.Element {
  return (
    <Typography variant="bodySmall" component="p" data-testid={testId}>
      <Box component="span" sx={{ fontWeight: 600 }}>
        {title}
      </Box>{' '}
      {items.length === 0 ? t('unattended.summary.nothing', 'nothing') : items.join(', ')}
    </Typography>
  );
}

/** The typed chain limits, else "platform defaults". */
function chainText(value: UnattendedGrantValue): string {
  const typed = CHAIN_PARTS.filter((part) => value.chain[part.field].trim() !== '').map((part) => `${part.label()}: ${value.chain[part.field].trim()}`);
  return typed.length === 0 ? t('unattended.summary.defaults', 'platform defaults (20 continuations, the spend cap of one run × 10, 12 hours)') : typed.join(', ');
}

export function UnattendedGrantSummary({ value, desktop = true, cloud = false }: UnattendedGrantSummaryProps): React.JSX.Element {
  const parts = visibleParts({ desktop, cloud });
  const answered = parts.filter((part) => value[part.key]).map((part) => part.label());
  const left = parts.filter((part) => !value[part.key]).map((part) => part.label());
  if (cloud && value.sensitive_tools.length > 0) answered.push(`${sensitiveLabel()}: ${value.sensitive_tools.map((named) => `${named.toolkit}/${named.tool}`).join(', ')}`);
  return (
    <Box data-testid="unattended-grant-summary" sx={{ display: 'flex', flexDirection: 'column', gap: 0.25 }}>
      <Line title={t('unattended.summary.answers', 'Answers:')} items={answered} testId="unattended-summary-answers" />
      {left.length > 0 && (
        <Typography variant="bodySmall" component="p" sx={muted} data-testid="unattended-summary-left">
          {t('unattended.summary.left', 'Not answered (the run takes the default at once): {{parts}}', { parts: left.join(', ') })}
        </Typography>
      )}
      <Line title={t('unattended.summary.chain', 'Chain limits:')} items={[chainText(value)]} testId="unattended-summary-chain" />
    </Box>
  );
}
