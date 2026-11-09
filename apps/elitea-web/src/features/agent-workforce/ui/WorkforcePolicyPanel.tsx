/**
 * Settings › Agent workforce (ADR-0050 decisions 5c and 5k, issue 1617):
 * the project's automatic hiring and dismissal policy, both off by default,
 * and the list of what the policy did, with undo and keep for a hire and
 * object for a retire notice.
 *
 * Only an agent manager edits the policy (`can_manage`); everyone who can
 * read agents sees it and the actions. The server is the authority on every
 * bound and on who may undo, keep or object: the page only offers the
 * buttons the action says are open and shows the server's refusal.
 */
import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import LinearProgress from '@mui/material/LinearProgress';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { AgentAutomaticAction, AgentWorkforcePolicy } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseSwitch } from '@/shared/ui/BaseSwitch';

import type { AutomaticActionDecision } from '../api/agentWorkforceApi';
import { actionKindLabel, actionStatusLabel, conditionText, formOf, inputOf, withThreshold, type PolicyForm } from '../lib/policy';
import { stageLabel, workforceRefusal } from '../lib/workforce';
import { useAutomaticActions, useWorkforcePolicy, useWorkforcePolicyActions } from '../model/useWorkforcePolicy';

export interface WorkforcePolicyPanelProps {
  readonly projectId: string;
}

const failed = (): string => t('features.agentWorkforce.decision.failed', 'That did not go through.');

function NumberField({
  label,
  value,
  disabled,
  testId,
  onChange,
}: {
  readonly label: string;
  readonly value: string;
  readonly disabled: boolean;
  readonly testId: string;
  readonly onChange: (value: string) => void;
}): ReactNode {
  return (
    <TextField
      size="small"
      type="number"
      label={label}
      value={value}
      disabled={disabled}
      onChange={(event) => onChange(event.target.value)}
      slotProps={{ htmlInput: { 'data-testid': testId } }}
      sx={{ maxWidth: '16rem' }}
    />
  );
}

function RulesEditor({
  form,
  disabled,
  onChange,
}: {
  readonly form: PolicyForm;
  readonly disabled: boolean;
  readonly onChange: (form: PolicyForm) => void;
}): ReactNode {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }} data-testid="agent-workforce-policy-rules">
      {form.rules.map((rule, ruleIndex) => (
        <Box key={rule.id} sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '0.5rem' }}>
          <Typography variant="labelMedium" sx={{ minWidth: '10rem' }}>
            {t('features.agentWorkforce.policy.rule', '{{rule}} → {{stage}}', { rule: rule.id, stage: stageLabel(rule.stage) })}
          </Typography>
          {rule.conditions.map((condition, conditionIndex) => (
            <TextField
              key={`${condition.metric}-${conditionIndex}`}
              size="small"
              type="number"
              label={conditionText(condition)}
              value={condition.value}
              disabled={disabled}
              onChange={(event) =>
                onChange({ ...form, rules: withThreshold(form.rules, ruleIndex, conditionIndex, Number(event.target.value)) })
              }
              slotProps={{ htmlInput: { 'data-testid': `agent-workforce-rule-${rule.id}-${conditionIndex}`, min: 0, step: 'any' } }}
              sx={{ maxWidth: '18rem' }}
            />
          ))}
        </Box>
      ))}
    </Box>
  );
}

function PolicyEditor({ projectId, policy }: { readonly projectId: string; readonly policy: AgentWorkforcePolicy }): ReactNode {
  const { save } = useWorkforcePolicyActions(projectId);
  const [form, setForm] = useState<PolicyForm>(() => formOf(policy));
  const [error, setError] = useState<string | undefined>(undefined);
  const [saved, setSaved] = useState(false);
  useEffect(() => setForm(formOf(policy)), [policy]);
  const disabled = !policy.can_manage || save.isPending;
  const input = inputOf(form);

  const submit = (): void => {
    if (input === undefined) return;
    setError(undefined);
    setSaved(false);
    save.mutate(input, {
      onSuccess: () => setSaved(true),
      onError: (failure) => setError(workforceRefusal(failure, failed()).message),
    });
  };
  const resetRules = (): void => setForm({ ...form, rules: policy.default_rules });

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }} data-testid="agent-workforce-policy">
      {!policy.can_manage && (
        <Alert severity="info">
          {t('features.agentWorkforce.policy.readOnly', 'Only an agent manager changes this policy.')}
        </Alert>
      )}
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
        <FormControlLabel
          control={
            <BaseSwitch
              checked={form.autoHireEnabled}
              disabled={disabled}
              onChange={(event) => setForm({ ...form, autoHireEnabled: event.target.checked })}
              aria-label={t('features.agentWorkforce.policy.autoHire', 'Hire proposed agents automatically')}
              data-testid="agent-workforce-policy-auto-hire"
            />
          }
          label={t('features.agentWorkforce.policy.autoHire', 'Hire proposed agents automatically')}
        />
        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'features.agentWorkforce.policy.autoHireHelp',
            'Only a low-tier, read-only proposal whose spec ran successfully enough times. The agent is created unpublished, with no tools bound, on a 14-day probation an agent manager can undo.',
          )}
        </Typography>
        <Box sx={{ display: 'flex', gap: '1rem', flexWrap: 'wrap' }}>
          <NumberField
            label={t('features.agentWorkforce.policy.minRuns', 'Successful runs needed (1–100)')}
            value={form.minSuccessfulRuns}
            disabled={disabled}
            testId="agent-workforce-policy-min-runs"
            onChange={(value) => setForm({ ...form, minSuccessfulRuns: value })}
          />
          <NumberField
            label={t('features.agentWorkforce.policy.maxHired', 'Agents on probation at once (1–50)')}
            value={form.maxAutoHired}
            disabled={disabled}
            testId="agent-workforce-policy-max-hired"
            onChange={(value) => setForm({ ...form, maxAutoHired: value })}
          />
        </Box>
        <Typography variant="bodySmall" color="text.secondary" data-testid="agent-workforce-policy-open-hired">
          {t('features.agentWorkforce.policy.openHired', 'On probation now: {{count}}', { count: policy.open_auto_hired })}
        </Typography>
      </Box>

      <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
        <FormControlLabel
          control={
            <BaseSwitch
              checked={form.autoDismissEnabled}
              disabled={disabled}
              onChange={(event) => setForm({ ...form, autoDismissEnabled: event.target.checked })}
              aria-label={t('features.agentWorkforce.policy.autoDismiss', 'Dismiss agents automatically by rule')}
              data-testid="agent-workforce-policy-auto-dismiss"
            />
          }
          label={t('features.agentWorkforce.policy.autoDismiss', 'Dismiss agents automatically by rule')}
        />
        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'features.agentWorkforce.policy.autoDismissHelp',
            'A matching rule moves an agent to watch or suspend at once. Retiring waits for the grace period, during which the owner can object. The policy never demotes or deletes, never goes past retire, and leaves critical-tier agents and agents under legal hold alone.',
          )}
        </Typography>
        <NumberField
          label={t('features.agentWorkforce.policy.grace', 'Grace before retiring, in days (1–90)')}
          value={form.graceDays}
          disabled={disabled}
          testId="agent-workforce-policy-grace"
          onChange={(value) => setForm({ ...form, graceDays: value })}
        />
        <RulesEditor form={form} disabled={disabled} onChange={setForm} />
      </Box>

      {error !== undefined && (
        <Alert severity="error" data-testid="agent-workforce-policy-error">
          {error}
        </Alert>
      )}
      {saved && error === undefined && (
        <Alert severity="success" onClose={() => setSaved(false)}>
          {t('features.agentWorkforce.policy.saved', 'Policy saved.')}
        </Alert>
      )}
      {policy.can_manage && (
        <Box sx={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="contained" disabled={disabled || input === undefined} onClick={submit} data-testid="agent-workforce-policy-save">
            {t('features.agentWorkforce.policy.save', 'Save policy')}
          </Button>
          <Button variant="outlined" disabled={disabled} onClick={resetRules}>
            {t('features.agentWorkforce.policy.resetRules', 'Use the default rules')}
          </Button>
        </Box>
      )}
    </Box>
  );
}

interface ActionButtonsProps {
  readonly action: AgentAutomaticAction;
  readonly canManage: boolean;
  readonly needsConfirm: boolean;
  readonly busy: boolean;
  readonly onRun: (decision: AutomaticActionDecision, confirmNested?: boolean) => void;
}

/** The decisions still open on one action: undo and keep a hire, object to a retire notice. */
function ActionButtons({ action, canManage, needsConfirm, busy, onRun }: ActionButtonsProps): ReactNode {
  return (
    <Box sx={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap' }}>
      {action.can_undo && canManage && (
        <Button size="small" variant="outlined" disabled={busy} onClick={() => onRun('undo', needsConfirm)} data-testid={`agent-workforce-action-undo-${action.id}`}>
          {needsConfirm
            ? t('features.agentWorkforce.automatic.undoNested', 'Undo anyway (agents that nest it will fail)')
            : t('features.agentWorkforce.automatic.undo', 'Undo the hire')}
        </Button>
      )}
      {/* Keep stays open after the undo window: an unreviewed hire counts against max_auto_hired until kept. */}
      {action.kind === 'auto_hire' && action.status === 'applied' && canManage && (
        <Button size="small" variant="outlined" disabled={busy} onClick={() => onRun('keep')} data-testid={`agent-workforce-action-keep-${action.id}`}>
          {t('features.agentWorkforce.automatic.keep', 'Keep the agent')}
        </Button>
      )}
      {action.can_object && (
        <Button size="small" variant="outlined" disabled={busy} onClick={() => onRun('object')} data-testid={`agent-workforce-action-object-${action.id}`}>
          {t('features.agentWorkforce.automatic.object', 'Object')}
        </Button>
      )}
    </Box>
  );
}

function ActionRow({ projectId, action, canManage }: { readonly projectId: string; readonly action: AgentAutomaticAction; readonly canManage: boolean }): ReactNode {
  const { decide } = useWorkforcePolicyActions(projectId);
  const [error, setError] = useState<string | undefined>(undefined);
  const [needsConfirm, setNeedsConfirm] = useState(false);

  const run = (decision: AutomaticActionDecision, confirmNested = false): void => {
    setError(undefined);
    decide.mutate(
      { actionId: action.id, decision, confirmNested },
      {
        onSuccess: () => setNeedsConfirm(false),
        onError: (failure) => {
          const refusal = workforceRefusal(failure, failed());
          if (refusal.code === 'workforce_confirm_nested') setNeedsConfirm(true);
          setError(refusal.message);
        },
      },
    );
  };
  const due = action.due_at === null ? '' : new Date(action.due_at).toLocaleString();

  return (
    <Box
      data-testid={`agent-workforce-action-${action.id}`}
      sx={{ display: 'flex', flexDirection: 'column', gap: '0.25rem', borderBottom: '1px solid', borderColor: 'divider', py: '0.5rem' }}
    >
      <Typography variant="labelMedium">
        {t('features.agentWorkforce.automatic.heading', '{{agent}}: {{kind}} — {{status}}', {
          agent: action.application_name,
          kind: actionKindLabel(action),
          status: actionStatusLabel(action.status),
        })}
      </Typography>
      {due !== '' && (action.can_undo || action.can_object || action.status === 'flagged') && (
        <Typography variant="bodySmall" color="text.secondary">
          {t('features.agentWorkforce.automatic.due', 'Until {{due}}', { due })}
        </Typography>
      )}
      {action.note !== null && (
        <Typography variant="bodySmall" color="text.secondary" sx={{ overflowWrap: 'anywhere' }}>
          {action.note}
        </Typography>
      )}
      <ActionButtons action={action} canManage={canManage} needsConfirm={needsConfirm} busy={decide.isPending} onRun={run} />
      {error !== undefined && (
        <Typography variant="bodySmall" color="error.main" data-testid={`agent-workforce-action-error-${action.id}`}>
          {error}
        </Typography>
      )}
    </Box>
  );
}

function ActionsList({ projectId, canManage }: { readonly projectId: string; readonly canManage: boolean }): ReactNode {
  const actions = useAutomaticActions(projectId);
  const rows = actions.data?.rows ?? [];
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }} data-testid="agent-workforce-actions">
      <Typography variant="headingSmall">{t('features.agentWorkforce.automatic.title', 'Automatic actions')}</Typography>
      {actions.isLoading && <LinearProgress />}
      {actions.isError && <Alert severity="error">{t('features.agentWorkforce.automatic.loadFailed', 'Failed to load the automatic actions.')}</Alert>}
      {actions.isSuccess && rows.length === 0 && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="agent-workforce-actions-empty">
          {t('features.agentWorkforce.automatic.empty', 'The policy has not done anything yet.')}
        </Typography>
      )}
      {rows.map((action) => (
        <ActionRow key={action.id} projectId={projectId} action={action} canManage={canManage} />
      ))}
    </Box>
  );
}

export function WorkforcePolicyPanel({ projectId }: WorkforcePolicyPanelProps): ReactNode {
  const policy = useWorkforcePolicy(projectId);
  if (projectId === '') return null;
  if (policy.isLoading) return <LinearProgress />;
  if (policy.isError || policy.data === undefined) {
    return <Alert severity="error">{t('features.agentWorkforce.policy.loadFailed', 'Failed to load the workforce policy.')}</Alert>;
  }
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      <PolicyEditor projectId={projectId} policy={policy.data} />
      <ActionsList projectId={projectId} canManage={policy.data.can_manage} />
    </Box>
  );
}
