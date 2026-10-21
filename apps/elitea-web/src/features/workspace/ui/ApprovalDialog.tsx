/**
 * The agent asked to do something that needs the user's say-so (run a command,
 * touch paths). One request at a time; the rest wait in the turn's queue.
 *
 * Deny is the focused button and Escape denies: the safe answer is the one
 * the keyboard reaches without thought. The backdrop does not dismiss, since
 * an accidental click must not answer for the user.
 *
 * The `continue` kind (the run used its steps: go on?) is not a modal: it is
 * a compact card the page puts in the transcript, under what the agent did.
 * It is asked every time and never remembered, so it offers no "always".
 * The `mcp_authorization` kind (an MCP server needs the person's sign-in) is
 * such a card too: McpAuthorizationCard.
 *
 * "Decline and tell the agent why" denies with the person's text
 * (`deny_with_feedback`): the agent reads it with the denial and can try
 * another way. The text goes only with a denial (IPC.md "Decline with
 * feedback").
 */
import { useEffect, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import type { ApprovalDecision, ApprovalRequestPayload } from '@/shared/desktop/workspaceIpc';

import { ruleSourceLabel } from '@/shared/desktop/ruleSource';

import { McpAuthorizationCard } from './McpAuthorizationCard';
import { MAX_FEEDBACK_CHARS } from './PlanCard';

export interface ApprovalDialogProps {
  /** The request being asked; `undefined` closes the dialog. */
  request: ApprovalRequestPayload | undefined;
  /** How many more are waiting behind this one. */
  queued: number;
  /** `feedback` comes only with `deny_with_feedback`. */
  onRespond: (requestId: string, decision: ApprovalDecision, feedback?: string) => void;
}

const monoSx = (theme: Theme) => ({
  margin: 0,
  padding: 1,
  overflowX: 'auto',
  whiteSpace: 'pre-wrap',
  wordBreak: 'break-all',
  fontFamily: theme.typography.fontFamilyMono,
  border: `1px solid ${theme.vars.palette.divider}`,
  borderRadius: theme.vars.shape.radiusMd,
});

/** Shell-quote an argv element only when it needs it, so the line reads like what would be typed. */
function quoteArg(arg: string): string {
  return /^[\w@%+=:,./-]+$/.test(arg) ? arg : `'${arg.replaceAll("'", "'\\''")}'`;
}

/** "The agent used its N steps": continue for N more, or stop here. */
function ContinueCard({ request, respond }: { request: ApprovalRequestPayload; respond: (decision: ApprovalDecision) => void }): React.JSX.Element {
  const steps = request.steps ?? 0;
  const titleId = `workspace-continue-${request.request_id}`;
  return (
    <Box
      component="section"
      aria-labelledby={titleId}
      data-testid="approval-continue-card"
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
        {t('workspace.continue.title', 'The agent used its {{n}} steps', { n: steps })}
      </Typography>
      {request.detail !== '' && (
        <Typography variant="bodySmall" sx={{ color: (theme: Theme) => theme.vars.palette.text.secondary, whiteSpace: 'pre-wrap' }}>
          {request.detail}
        </Typography>
      )}
      <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap' }}>
        <Button size="small" variant="contained" onClick={() => respond('allow_once')}>
          {t('workspace.continue.more', 'Continue for {{n}} more steps', { n: steps })}
        </Button>
        <Button size="small" onClick={() => respond('deny')}>
          {t('workspace.continue.stop', 'Stop here')}
        </Button>
      </Box>
    </Box>
  );
}

/** The command and the paths a request names, each shown only when present. */
function ApprovalTargets({ command, paths }: { command?: string[] | undefined; paths?: string[] | undefined }): React.JSX.Element {
  return (
    <>
      {command !== undefined && command.length > 0 && (
        <Box component="pre" data-testid="approval-command" aria-label={t('workspace.approval.command', 'Command')} sx={monoSx}>
          {command.map(quoteArg).join(' ')}
        </Box>
      )}
      {paths !== undefined && paths.length > 0 && (
        <Box component="ul" data-testid="approval-paths" aria-label={t('workspace.approval.paths', 'Paths')} sx={(theme: Theme) => ({ ...monoSx(theme), listStyle: 'none' })}>
          {paths.map((path) => (
            <li key={path}>{path}</li>
          ))}
        </Box>
      )}
    </>
  );
}

/** The box the person writes the agent a reason in, once they chose "Decline and tell the agent why". */
function FeedbackBox({ value, onChange }: { value: string; onChange: (value: string) => void }): React.JSX.Element {
  const inputRef = useRef<HTMLTextAreaElement>(null);
  // The box opens on the person's click: their next keys go into it.
  useEffect(() => inputRef.current?.focus(), []);
  return (
    <TextField
      size="small"
      multiline
      minRows={2}
      inputRef={inputRef}
      value={value}
      onChange={(event) => onChange(event.target.value.slice(0, MAX_FEEDBACK_CHARS))}
      label={t('workspace.approval.feedback', 'Tell the agent why (and what to do instead)')}
      slotProps={{ htmlInput: { maxLength: MAX_FEEDBACK_CHARS, 'data-testid': 'approval-feedback' } }}
    />
  );
}

export function ApprovalDialog(props: ApprovalDialogProps): React.JSX.Element | null {
  // A new request starts with no feedback box open.
  return <ApprovalPrompt key={props.request?.request_id ?? ''} {...props} />;
}

function ApprovalPrompt({ request, queued, onRespond }: ApprovalDialogProps): React.JSX.Element | null {
  if (request === undefined) return null;
  const respond = (decision: ApprovalDecision): void => onRespond(request.request_id, decision);
  if (request.kind === 'continue') return <ContinueCard request={request} respond={respond} />;
  if (request.kind === 'mcp_authorization') return <McpAuthorizationCard request={request} respond={respond} />;
  return <ToolApproval request={request} queued={queued} onRespond={onRespond} />;
}

/** A tool's approval: the modal dialog (mounted once per request). */
function ToolApproval({ request, queued, onRespond }: ApprovalDialogProps & { request: ApprovalRequestPayload }): React.JSX.Element {
  const denyRef = useRef<HTMLButtonElement>(null);
  const [feedback, setFeedback] = useState<string | null>(null);
  // Deny takes focus when the request is shown (focus-trap order would otherwise start at the first button).
  useEffect(() => denyRef.current?.focus(), []);
  const respond = (decision: ApprovalDecision): void => onRespond(request.request_id, decision);
  const decidedBy = ruleSourceLabel(request.source, request.rule);
  const explained = feedback?.trim() ?? '';
  const declineWithFeedback = (): void => {
    if (explained === '') respond('deny');
    else onRespond(request.request_id, 'deny_with_feedback', explained);
  };
  return (
    <Dialog
      open
      maxWidth="sm"
      fullWidth
      aria-labelledby="workspace-approval-title"
      aria-describedby="workspace-approval-detail"
      // The focus trap claims focus after mount; Deny takes it once the dialog has finished opening (unless the person already opened the reason box).
      slotProps={{ transition: { onEntered: () => feedback === null && denyRef.current?.focus() } }}
      onClose={(_event, reason) => {
        if (reason === 'escapeKeyDown') respond('deny');
      }}
    >
      <DialogTitle id="workspace-approval-title">{request.title}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
        <Typography id="workspace-approval-detail" variant="bodyMedium">
          {request.detail}
        </Typography>
        <ApprovalTargets command={request.command} paths={request.paths} />
        <Typography variant="bodySmall" sx={{ color: (theme: Theme) => theme.vars.palette.text.secondary }}>
          {t('workspace.approval.reason', 'Why: {{reason}}', { reason: request.justification ?? request.reason })}
        </Typography>
        {decidedBy !== undefined && (
          <Typography variant="bodySmall" data-testid="approval-rule-source" sx={{ color: (theme: Theme) => theme.vars.palette.text.secondary }}>
            {t('workspace.approval.source', 'Asked by {{source}}', { source: decidedBy })}
          </Typography>
        )}
        {queued > 0 && (
          <Typography variant="bodySmall" component="output">
            {t('workspace.approval.queued', '{{n}} more waiting', { n: queued })}
          </Typography>
        )}
        {feedback !== null && <FeedbackBox value={feedback} onChange={setFeedback} />}
      </DialogContent>
      <DialogActions>
        {feedback === null ? (
          <Button color="error" onClick={() => setFeedback('')}>
            {t('workspace.approval.declineWhy', 'Decline and tell the agent why')}
          </Button>
        ) : (
          <Button color="error" variant="outlined" onClick={declineWithFeedback}>
            {t('workspace.approval.sendDecline', 'Decline and send')}
          </Button>
        )}
        <Button ref={denyRef} color="error" onClick={() => respond('deny')}>
          {t('workspace.approval.deny', 'Deny')}
        </Button>
        <Button variant="outlined" onClick={() => respond('allow_once')}>
          {t('workspace.approval.allowOnce', 'Allow once')}
        </Button>
        {request.can_remember && (
          <Button variant="contained" onClick={() => respond('allow_always')}>
            {t('workspace.approval.allowAlways', 'Always allow')}
          </Button>
        )}
      </DialogActions>
    </Dialog>
  );
}
