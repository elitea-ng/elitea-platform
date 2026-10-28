/**
 * The Suspend dialog of `Admin › Kill switch` (ADR-0041 decision 1).
 *
 * One form for the four scopes. The platform scope stops every run on the
 * platform, so it also needs the typed confirmation STOP; the server refuses
 * it without that word too (`confirmation_required`), so this check is
 * presentation and the route is the rule.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import MenuItem from '@mui/material/MenuItem';
import Stack from '@mui/material/Stack';
import TextField from '@mui/material/TextField';

import type { ExecutionSuspensionCreate } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

export const SUSPENSION_SCOPES = ['agent', 'project', 'toolkit_type', 'platform'] as const;
type Scope = (typeof SUSPENSION_SCOPES)[number];
const RESUME_RULES = ['any_admin', 'governance_role', 'two_person'] as const;

/** The typed confirmation the platform scope needs. */
export const PLATFORM_STOP_CONFIRMATION = 'STOP';

export function scopeLabel(scope: string): string {
  switch (scope) {
    case 'agent':
      return t('pages.admin.killSwitch.scopeName.agent', 'Agent');
    case 'project':
      return t('pages.admin.killSwitch.scopeName.project', 'Project');
    case 'toolkit_type':
      return t('pages.admin.killSwitch.scopeName.toolkitType', 'Toolkit type');
    case 'platform':
      return t('pages.admin.killSwitch.scopeName.platform', 'Platform');
    default:
      return scope;
  }
}

export function resumeRuleLabel(rule: string): string {
  switch (rule) {
    case 'any_admin':
      return t('pages.admin.killSwitch.resumeRule.anyAdmin', 'Any administrator');
    case 'governance_role':
      return t('pages.admin.killSwitch.resumeRule.governanceRole', 'A platform administrator');
    case 'two_person':
      return t('pages.admin.killSwitch.resumeRule.twoPerson', 'A second platform administrator');
    default:
      return rule;
  }
}

interface FormState {
  scope: Scope;
  projectId: string;
  applicationId: string;
  toolkitType: string;
  reason: string;
  resumeRequires: string;
  confirm: string;
}

const EMPTY: FormState = {
  scope: 'agent',
  projectId: '',
  applicationId: '',
  toolkitType: '',
  reason: '',
  resumeRequires: 'any_admin',
  confirm: '',
};

/** The largest project or agent id the API accepts (a PostgreSQL integer). */
export const MAX_SUSPENSION_ID = 2_147_483_647;

const positive = (value: string): number | undefined => {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 && parsed <= MAX_SUSPENSION_ID ? parsed : undefined;
};

/** The request body, or undefined while the form is not complete. */
export function suspensionBody(form: FormState): ExecutionSuspensionCreate | undefined {
  const reason = form.reason.trim();
  if (reason === '') return undefined;
  const base = { scope: form.scope, reason, resume_requires: form.resumeRequires };
  switch (form.scope) {
    case 'platform':
      return form.confirm === PLATFORM_STOP_CONFIRMATION ? { ...base, confirm: form.confirm } : undefined;
    case 'project': {
      const projectId = positive(form.projectId);
      return projectId === undefined ? undefined : { ...base, project_id: projectId };
    }
    case 'agent': {
      const projectId = positive(form.projectId);
      const applicationId = positive(form.applicationId);
      return projectId === undefined || applicationId === undefined
        ? undefined
        : { ...base, project_id: projectId, application_id: applicationId };
    }
    case 'toolkit_type': {
      const toolkitType = form.toolkitType.trim().toLowerCase();
      return toolkitType === '' ? undefined : { ...base, toolkit_type: toolkitType };
    }
    default:
      return form.scope satisfies never;
  }
}

/**
 * What a suspension of the scope does to runs. Only agent, project and
 * platform suspensions cancel the runs in flight; a toolkit type suspension
 * refuses new admissions, and an agent run already in flight keeps the
 * guardrails it froze at admission until it ends.
 */
function scopeEffect(scope: Exclude<Scope, 'platform'>): string {
  switch (scope) {
    case 'agent':
      return t('pages.admin.killSwitch.effect.agent', 'New runs of this agent are refused, and its runs in flight are cancelled.');
    case 'project':
      return t('pages.admin.killSwitch.effect.project', 'New runs of this project are refused, and its runs in flight are cancelled.');
    case 'toolkit_type':
      return t(
        'pages.admin.killSwitch.effect.toolkitType',
        'New runs and new tool calls through admission are refused; runs already in flight keep the toolkit until they end.',
      );
    default:
      return scope satisfies never;
  }
}

export interface AdminSuspendDialogProps {
  readonly open: boolean;
  readonly pending: boolean;
  readonly error: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (body: ExecutionSuspensionCreate) => void;
}

export function AdminSuspendDialog({ open, pending, error, onClose, onSubmit }: AdminSuspendDialogProps): ReactNode {
  const [form, setForm] = useState<FormState>(EMPTY);
  const set = (patch: Partial<FormState>): void => setForm((current) => ({ ...current, ...patch }));
  const body = suspensionBody(form);
  const close = (): void => {
    setForm(EMPTY);
    onClose();
  };

  return (
    <Dialog open={open} onClose={close} fullWidth maxWidth="sm">
      <DialogTitle>{t('pages.admin.killSwitch.suspendTitle', 'Suspend execution')}</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ paddingTop: 1 }}>
          <TextField
            select
            label={t('pages.admin.killSwitch.scope', 'Scope')}
            value={form.scope}
            slotProps={{ htmlInput: { 'data-testid': 'kill-switch-scope' } }}
            onChange={(event) => set({ scope: event.target.value as Scope, confirm: '' })}
          >
            {SUSPENSION_SCOPES.map((scope) => (
              <MenuItem key={scope} value={scope}>{scopeLabel(scope)}</MenuItem>
            ))}
          </TextField>
          {(form.scope === 'agent' || form.scope === 'project') && (
            <TextField
              label={t('pages.admin.killSwitch.projectId', 'Project ID')}
              value={form.projectId}
              onChange={(event) => set({ projectId: event.target.value })}
            />
          )}
          {form.scope === 'agent' && (
            <TextField
              label={t('pages.admin.killSwitch.applicationId', 'Agent ID')}
              value={form.applicationId}
              onChange={(event) => set({ applicationId: event.target.value })}
            />
          )}
          {form.scope === 'toolkit_type' && (
            <TextField
              label={t('pages.admin.killSwitch.toolkitType', 'Toolkit type')}
              helperText={t('pages.admin.killSwitch.toolkitTypeHelp', 'For example github or jira.')}
              value={form.toolkitType}
              onChange={(event) => set({ toolkitType: event.target.value })}
            />
          )}
          <TextField
            label={t('pages.admin.killSwitch.reason', 'Reason')}
            value={form.reason}
            multiline
            slotProps={{ htmlInput: { maxLength: 1024 } }}
            onChange={(event) => set({ reason: event.target.value })}
          />
          <TextField
            select
            label={t('pages.admin.killSwitch.resumeRequires', 'Who can resume')}
            value={form.resumeRequires}
            onChange={(event) => set({ resumeRequires: event.target.value })}
          >
            {RESUME_RULES.map((rule) => (
              <MenuItem key={rule} value={rule}>{resumeRuleLabel(rule)}</MenuItem>
            ))}
          </TextField>
          {form.scope !== 'platform' && (
            <Alert severity="info" data-testid="kill-switch-effect">
              {scopeEffect(form.scope)}
            </Alert>
          )}
          {form.scope === 'platform' && (
            <>
              <Alert severity="error">
                {t(
                  'pages.admin.killSwitch.platformWarning',
                  'This stops every run on the platform and cancels every run in flight. Type STOP to confirm.',
                )}
              </Alert>
              <TextField
                label={t('pages.admin.killSwitch.confirm', 'Type STOP')}
                value={form.confirm}
                slotProps={{ htmlInput: { 'data-testid': 'kill-switch-confirm' } }}
                onChange={(event) => set({ confirm: event.target.value })}
              />
            </>
          )}
          {error !== undefined && <Alert severity="error">{error}</Alert>}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={close}>{t('common.cancel', 'Cancel')}</Button>
        <Button
          color="error"
          variant="contained"
          data-testid="kill-switch-submit"
          disabled={body === undefined || pending}
          onClick={() => {
            if (body !== undefined) onSubmit(body);
          }}
        >
          {t('pages.admin.killSwitch.suspendConfirm', 'Suspend')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
