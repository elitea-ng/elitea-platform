/**
 * The agent's checklist (the runtime planner's `update_plan`, as
 * `plan_updated` sends it) in the thread's inspector: the title, the
 * agent's note, progress, and each step with its status. The plan is
 * model-written text: shown as plain text, never as markup.
 *
 * Loaded lazily (the session's Plan tab): nothing of it is in the first
 * bundle.
 */
import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { PlanStep, PlanView } from '@/shared/desktop/planIpc';
import { t } from '@/shared/i18n';

export interface TodoPaneProps {
  /** The thread's latest plan; `null` when the agent keeps none. */
  plan: PlanView | null;
}

const MARK: Readonly<Record<PlanStep['status'], string>> = {
  pending: '☐',
  in_progress: '▶',
  completed: '☑',
  cancelled: '☒',
};

function statusLabel(status: PlanStep['status']): string {
  switch (status) {
    case 'pending':
      return t('workspace.todo.pending', 'To do');
    case 'in_progress':
      return t('workspace.todo.inProgress', 'In progress');
    case 'completed':
      return t('workspace.todo.completed', 'Done');
    case 'cancelled':
      return t('workspace.todo.cancelled', 'Cancelled');
  }
}

function Step({ step }: { step: PlanStep }): React.JSX.Element {
  const done = step.status === 'completed' || step.status === 'cancelled';
  return (
    <Box component="li" data-testid="todo-step" data-status={step.status} sx={{ display: 'flex', gap: 1, paddingY: 0.25, alignItems: 'baseline' }}>
      <Typography variant="bodySmall" aria-hidden sx={{ width: '1.25em', flexShrink: 0 }}>
        {MARK[step.status]}
      </Typography>
      <Typography
        variant="bodySmall"
        sx={(theme: Theme) => ({
          flex: 1,
          minWidth: 0,
          overflowWrap: 'anywhere',
          fontWeight: step.status === 'in_progress' ? 600 : undefined,
          textDecoration: step.status === 'cancelled' ? 'line-through' : undefined,
          color: done ? theme.vars.palette.text.secondary : undefined,
        })}
      >
        {step.step}
        <Box component="span" sx={{ position: 'absolute', width: 1, height: 1, overflow: 'hidden', clip: 'rect(0 0 0 0)' }}>
          {` (${statusLabel(step.status)})`}
        </Box>
      </Typography>
    </Box>
  );
}

export function TodoPane({ plan }: TodoPaneProps): React.JSX.Element {
  if (plan === null || plan.steps.length === 0) {
    return (
      <Typography variant="bodySmall" data-testid="todo-empty" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {t('workspace.todo.empty', 'No plan yet. When the agent keeps a checklist for its work, it shows up here.')}
      </Typography>
    );
  }
  const completed = plan.steps.filter((step) => step.status === 'completed').length;
  return (
    <Box component="section" data-testid="todo-pane" sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      <Typography variant="labelMedium" component="h2" sx={{ margin: 0, overflowWrap: 'anywhere' }}>
        {plan.title}
      </Typography>
      {plan.explanation !== undefined && plan.explanation !== '' && (
        <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' })}>
          {plan.explanation}
        </Typography>
      )}
      <Typography variant="bodySmall" component="output" data-testid="todo-progress" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {t('workspace.todo.progress', '{{done}} of {{total}} done', { done: completed, total: plan.steps.length })}
      </Typography>
      <Box component="ol" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
        {plan.steps.map((step) => (
          <Step key={step.n} step={step} />
        ))}
      </Box>
    </Box>
  );
}
