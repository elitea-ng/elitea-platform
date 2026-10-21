/**
 * The plan the agent proposed in plan mode (`plan_proposed`, its
 * `exit_plan_mode`), as a card in the transcript: the plan (Markdown the
 * model wrote, rendered without HTML), Accept, and Decline with a text box
 * that tells the agent what to change.
 *
 * Only Accept leaves plan mode, and only this card's Accept: no rule or
 * remembered choice answers a plan, so the card offers no "always". The
 * feedback goes only with Decline, never with Accept (IPC.md "Plan mode").
 */
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { PlanProposalPayload } from '@/shared/desktop/planIpc';
import type { ApprovalDecision } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { Markdown } from '@/shared/ui/Markdown';

/** What the host keeps of the person's text (2 KiB); the box stops at it too. */
export const MAX_FEEDBACK_CHARS = 2048;

export interface PlanCardProps {
  /** The plan waiting for an answer; `undefined` renders nothing. */
  proposal: PlanProposalPayload | undefined;
  onRespond: (requestId: string, decision: ApprovalDecision, feedback?: string) => void;
}

export function PlanCard(props: PlanCardProps): React.JSX.Element | null {
  // A new proposal starts with an empty box.
  return <PlanPrompt key={props.proposal?.request_id ?? ''} {...props} />;
}

function PlanPrompt({ proposal, onRespond }: PlanCardProps): React.JSX.Element | null {
  const [feedback, setFeedback] = useState('');
  if (proposal === undefined) return null;
  const titleId = `workspace-plan-${proposal.request_id}`;
  const decline = (): void => {
    const text = feedback.trim();
    if (text === '') onRespond(proposal.request_id, 'decline');
    else onRespond(proposal.request_id, 'decline', text);
  };
  return (
    <Box
      component="section"
      aria-labelledby={titleId}
      data-testid="plan-card"
      sx={(theme: Theme) => ({
        display: 'flex',
        flexDirection: 'column',
        gap: 1.5,
        padding: 1.5,
        border: `1px solid ${theme.vars.palette.divider}`,
        borderRadius: theme.vars.shape.radiusMd,
      })}
    >
      <Typography id={titleId} variant="labelMedium" component="h3" sx={{ margin: 0 }}>
        {t('workspace.plan.proposed', 'The agent proposes this plan')}
      </Typography>
      <Box data-testid="plan-card-plan" sx={{ maxHeight: 360, overflowY: 'auto' }}>
        <Markdown renderHtml={false}>{proposal.plan}</Markdown>
      </Box>
      <TextField
        size="small"
        multiline
        minRows={2}
        value={feedback}
        onChange={(event) => setFeedback(event.target.value.slice(0, MAX_FEEDBACK_CHARS))}
        label={t('workspace.plan.feedback', 'What should change? (sent only if you decline)')}
        slotProps={{ htmlInput: { maxLength: MAX_FEEDBACK_CHARS, 'data-testid': 'plan-card-feedback' } }}
      />
      <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap' }}>
        <Button size="small" variant="contained" onClick={() => onRespond(proposal.request_id, 'accept')}>
          {t('workspace.plan.accept', 'Accept plan')}
        </Button>
        <Button size="small" onClick={decline}>
          {t('workspace.plan.decline', 'Decline')}
        </Button>
      </Box>
      <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}>
        {t('workspace.plan.acceptHint', 'Accepting leaves plan mode: the agent goes on with every tool, asking as usual.')}
      </Typography>
    </Box>
  );
}
