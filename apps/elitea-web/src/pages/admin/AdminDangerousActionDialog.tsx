/**
 * The one confirmation of a dangerous action in the console (ADR-0059
 * decision 6, "Dangerous actions"; R3.8b, issue 2614).
 *
 * Every `dangerous` and `critical` action of the action table that has a
 * console control opens this dialog. It says what changes, how many projects
 * or people it reaches ("unknown" when the page cannot tell, never zero), asks
 * for the typed name of the target, a reason code and a reason text, and it
 * blocks until the typed name matches. The reason it hands to `onConfirm` is
 * what the API module sends (`adminDangerousAction.ts`): the body fields
 * `reason_code` and `reason`, or the `Elitea-Reason*` headers.
 *
 * The text is cut to the header's bound (500 bytes of UTF-8) as it is typed,
 * with a visible counter, so a long reason never fails the action.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Stack from '@mui/material/Stack';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { ActionReasonCode } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import {
  ADMIN_REASON_CODES,
  MAX_REASON_BYTES,
  REASON_CODE_OTHER,
  clampReason,
  dangerLevel,
  reasonBytes,
  type AdminReason,
  type DangerousActionKey,
} from './adminDangerousAction';

/** How far the action reaches: `count` undefined is "unknown", never zero. */
export interface AdminDangerReach {
  readonly count: number | undefined;
  readonly unit: 'projects' | 'people';
}

export interface AdminDangerousActionDialogProps {
  /** Open while an action waits for its confirmation. */
  readonly open: boolean;
  /** The action table key: it sets the level the dialog shows. */
  readonly action: DangerousActionKey;
  readonly title: string;
  /** What changes, in a sentence or two. */
  readonly change: ReactNode;
  readonly reach: AdminDangerReach;
  /** The name the operator types to arm the confirmation. */
  readonly target: string;
  readonly confirmLabel: string;
  readonly pending: boolean;
  readonly error?: string | null | undefined;
  readonly onClose: () => void;
  readonly onConfirm: (reason: AdminReason) => void;
  /** Extra facts the page knows (a list of the targets, a partial failure). */
  readonly children?: ReactNode;
}

function codeLabel(code: string): string {
  switch (code) {
    case 'incident':
      return t('pages.admin.dangerous.code.incident', 'Incident');
    case 'request':
      return t('pages.admin.dangerous.code.request', 'Request');
    case 'maintenance':
      return t('pages.admin.dangerous.code.maintenance', 'Maintenance');
    case 'correction':
      return t('pages.admin.dangerous.code.correction', 'Correction');
    case 'offboarding':
      return t('pages.admin.dangerous.code.offboarding', 'Offboarding');
    case REASON_CODE_OTHER:
      return t('pages.admin.dangerous.code.other', 'Other');
    default:
      return code;
  }
}

function reachText(reach: AdminDangerReach): string {
  if (reach.count === undefined) {
    return reach.unit === 'projects'
      ? t('pages.admin.dangerous.reach.projectsUnknown', 'Projects reached: unknown')
      : t('pages.admin.dangerous.reach.peopleUnknown', 'People reached: unknown');
  }
  return reach.unit === 'projects'
    ? t('pages.admin.dangerous.reach.projects', 'Projects reached: {{count}}', { count: reach.count })
    : t('pages.admin.dangerous.reach.people', 'People reached: {{count}}', { count: reach.count });
}

function LevelSummary({ action, change, reach }: Pick<AdminDangerousActionDialogProps, 'action' | 'change' | 'reach'>) {
  const critical = dangerLevel(action) === 'critical';
  return (
    <Alert severity={critical ? 'error' : 'warning'} data-testid="admin-dangerous-summary">
      <Stack spacing={1}>
        <Chip
          size="small"
          color={critical ? 'error' : 'warning'}
          sx={{ alignSelf: 'flex-start' }}
          data-testid="admin-dangerous-level"
          label={
            critical
              ? t('pages.admin.dangerous.level.critical', 'Critical action')
              : t('pages.admin.dangerous.level.dangerous', 'Dangerous action')
          }
        />
        <Typography variant="bodyMedium" component="div">
          {change}
        </Typography>
        <Typography variant="bodyMedium" data-testid="admin-dangerous-reach">
          {reachText(reach)}
        </Typography>
      </Stack>
    </Alert>
  );
}

interface ReasonFieldsProps {
  readonly code: ActionReasonCode | '';
  readonly text: string;
  readonly disabled: boolean;
  readonly onCode: (code: ActionReasonCode | '') => void;
  readonly onText: (text: string) => void;
}

function ReasonFields({ code, text, disabled, onCode, onText }: ReasonFieldsProps) {
  const needsText = code === REASON_CODE_OTHER && text.trim() === '';
  return (
    <>
      <TextField
        select
        fullWidth
        size="small"
        disabled={disabled}
        value={code}
        onChange={(event) => onCode(ADMIN_REASON_CODES.find((value) => value === event.target.value) ?? '')}
        label={t('pages.admin.dangerous.reasonCode', 'Reason')}
        slotProps={{
          select: { native: true },
          inputLabel: { shrink: true },
          htmlInput: { 'data-testid': 'admin-dangerous-reason-code' },
        }}
      >
        <option value="">{t('pages.admin.dangerous.reasonCodeChoose', 'Choose a reason')}</option>
        {ADMIN_REASON_CODES.map((value) => (
          <option key={value} value={value}>
            {codeLabel(value)}
          </option>
        ))}
      </TextField>
      <TextField
        fullWidth
        multiline
        minRows={2}
        size="small"
        disabled={disabled}
        value={text}
        error={needsText}
        onChange={(event) => onText(clampReason(event.target.value))}
        label={t('pages.admin.dangerous.reasonText', 'Why (recorded in the audit log)')}
        helperText={
          <span data-testid="admin-dangerous-reason-counter">
            {t('pages.admin.dangerous.reasonCounter', '{{used}} / {{max}} bytes', {
              used: reasonBytes(text),
              max: MAX_REASON_BYTES,
            })}
          </span>
        }
        slotProps={{ htmlInput: { 'data-testid': 'admin-dangerous-reason-text' } }}
      />
    </>
  );
}

function DialogBody(props: Omit<AdminDangerousActionDialogProps, 'open'>) {
  const { action, title, change, reach, target, confirmLabel, pending, error, onClose, onConfirm, children } = props;
  const [typed, setTyped] = useState('');
  const [code, setCode] = useState<ActionReasonCode | ''>('');
  const [text, setText] = useState('');
  const matches = typed.trim() === target.trim();
  const reasonReady = code !== '' && (code !== REASON_CODE_OTHER || text.trim() !== '');
  const armed = matches && reasonReady && !pending;
  return (
    <>
      <DialogTitle id="admin-dangerous-title">{title}</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ paddingTop: '0.25rem' }}>
          <LevelSummary action={action} change={change} reach={reach} />
          {children}
          <ReasonFields code={code} text={text} disabled={pending} onCode={setCode} onText={setText} />
          <TextField
            fullWidth
            size="small"
            disabled={pending}
            value={typed}
            onChange={(event) => setTyped(event.target.value)}
            label={t('pages.admin.dangerous.typeTarget', 'Type “{{target}}” to confirm', { target })}
            slotProps={{ htmlInput: { 'data-testid': 'admin-dangerous-target' } }}
          />
          {error ? (
            <Alert severity="error" data-testid="admin-dangerous-error">
              {error}
            </Alert>
          ) : null}
        </Stack>
      </DialogContent>
      <DialogActions sx={{ paddingX: '1.5rem', paddingBottom: '1rem' }}>
        <Button variant="elitea" color="tertiary" onClick={onClose} disabled={pending}>
          {t('pages.admin.dangerous.cancel', 'Cancel')}
        </Button>
        <Button
          variant="elitea"
          color="alarm"
          disabled={!armed}
          onClick={() => {
            if (code !== '') onConfirm({ code, text });
          }}
          data-testid="admin-dangerous-confirm"
        >
          {confirmLabel}
        </Button>
      </DialogActions>
    </>
  );
}

export function AdminDangerousActionDialog({ open, ...body }: AdminDangerousActionDialogProps) {
  return (
    <Dialog
      open={open}
      onClose={body.pending ? undefined : body.onClose}
      maxWidth="sm"
      fullWidth
      aria-labelledby="admin-dangerous-title"
      data-testid="admin-dangerous-dialog"
    >
      {/* Mounted only while open, so each opening starts with empty fields. */}
      {open ? <DialogBody {...body} /> : null}
    </Dialog>
  );
}
