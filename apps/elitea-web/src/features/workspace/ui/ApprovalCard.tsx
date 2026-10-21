/**
 * An approval (or a question) in the transcript, in the same native style as
 * the local agent's own cards ("The agent used its steps": a bordered inline
 * card, no elevation): what is being asked, the tool's parameters behind a
 * disclosure, and buttons by role — the default (primary) answer, and the
 * destructive one in the error colour.
 *
 * Presentational: the page decides what the buttons do (the local host's
 * approval route, or the cloud copy's resume route).
 */
import { useId, useState } from 'react';

import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import ButtonBase from '@mui/material/ButtonBase';
import Collapse from '@mui/material/Collapse';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { ruleSourceLabel } from '../model/ruleSource';

export interface ApprovalCardProps {
  /** What is asked: the tool's name or the question. */
  title: string;
  detail?: string | undefined;
  /** A rule's justification for asking (#1216), shown as "Why". */
  justification?: string | undefined;
  /** The layer that asks (`workspace`, `user`, `default`, …) and the rule (`workspace#3`), shown as "Asked by". */
  source?: string | undefined;
  rule?: string | undefined;
  /** The tool's parameters (shown behind "Parameters"); masked values arrive masked. */
  params?: Readonly<Record<string, unknown>> | undefined;
  /** The default answer. */
  approveLabel: string;
  onApprove: () => void;
  /** The destructive answer; absent for a prompt that only continues. */
  denyLabel?: string | undefined;
  onDeny?: (() => void) | undefined;
  disabled?: boolean;
  testId?: string;
}

const text = (value: unknown): string => (typeof value === 'string' ? value : JSON.stringify(value, null, 2));

function Params({ params }: { params: Readonly<Record<string, unknown>> }): React.JSX.Element | null {
  const [open, setOpen] = useState(false);
  const id = useId();
  const entries = Object.entries(params);
  if (entries.length === 0) return null;
  return (
    <Box>
      <ButtonBase
        aria-expanded={open}
        aria-controls={id}
        onClick={() => setOpen((value) => !value)}
        sx={(theme: Theme) => ({ gap: 0.5, borderRadius: theme.vars.shape.radiusSm, paddingX: 0.5, color: theme.vars.palette.text.secondary })}
      >
        {open ? <ExpandMoreIcon aria-hidden sx={{ fontSize: 14 }} /> : <ChevronRightIcon aria-hidden sx={{ fontSize: 14 }} />}
        <Typography variant="labelSmall" component="span">
          {t('workspace.approval.params', 'Parameters')}
        </Typography>
      </ButtonBase>
      <Collapse in={open} unmountOnExit>
        <Box
          id={id}
          component="pre"
          data-selectable
          sx={(theme: Theme) => ({
            margin: 0,
            marginTop: 0.5,
            padding: 1,
            overflowX: 'auto',
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-all',
            fontSize: '0.75rem',
            fontFamily: theme.typography.fontFamilyMono,
            border: `1px solid ${theme.vars.palette.divider}`,
            borderRadius: theme.vars.shape.radiusMd,
          })}
        >
          {entries.map(([key, value]) => `${key}: ${text(value)}`).join('\n')}
        </Box>
      </Collapse>
    </Box>
  );
}

export function ApprovalCard({ title, detail, justification, source, rule, params, approveLabel, onApprove, denyLabel, onDeny, disabled = false, testId = 'approval-card' }: ApprovalCardProps): React.JSX.Element {
  const titleId = useId();
  const askedBy = ruleSourceLabel(source, rule);
  return (
    <Box
      component="section"
      aria-labelledby={titleId}
      data-testid={testId}
      sx={(theme: Theme) => ({
        display: 'flex',
        flexDirection: 'column',
        gap: 1,
        padding: 1.5,
        border: `1px solid ${theme.vars.palette.divider}`,
        borderRadius: theme.vars.shape.radiusMd,
      })}
    >
      <Typography id={titleId} variant="labelMedium">
        {title}
      </Typography>
      {detail !== undefined && detail !== '' && (
        <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, whiteSpace: 'pre-wrap' })}>
          {detail}
        </Typography>
      )}
      {justification !== undefined && justification !== '' && (
        <Typography variant="bodySmall" data-testid="approval-justification" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, whiteSpace: 'pre-wrap' })}>
          {t('workspace.approval.reason', 'Why: {{reason}}', { reason: justification })}
        </Typography>
      )}
      {askedBy !== undefined && (
        <Typography variant="bodySmall" data-testid="approval-rule-source" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}>
          {t('workspace.approval.source', 'Asked by {{source}}', { source: askedBy })}
        </Typography>
      )}
      {params !== undefined && <Params params={params} />}
      <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap' }}>
        <Button size="small" variant="contained" disabled={disabled} onClick={onApprove}>
          {approveLabel}
        </Button>
        {denyLabel !== undefined && onDeny !== undefined && (
          <Button size="small" color="error" disabled={disabled} onClick={onDeny}>
            {denyLabel}
          </Button>
        )}
      </Box>
    </Box>
  );
}
