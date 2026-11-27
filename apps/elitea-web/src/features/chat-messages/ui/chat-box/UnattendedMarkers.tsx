/**
 * The marks of an unattended run in the chat transcript (ADR-0045 decision 4),
 * in the wording of the desktop transcript: "Continued automatically" on a
 * turn that Main started by itself, and the step-limit stop on an answer.
 */
import Alert from '@mui/material/Alert';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory.types';

/** Main started this turn by itself, the n-th time in its chain (when known). */
export function ContinuedAutomaticallyLabel({ index }: { readonly index?: number | undefined }) {
  return (
    <Typography variant="labelSmall" color="text.secondary" data-testid="chat-message-continued-automatically" sx={{ display: 'block', mt: 0.5 }}>
      {index === undefined
        ? t('features.chatMessages.continuedAutomatically', 'Continued automatically')
        : t('workspace.chain.continued', 'Continued automatically ({{n}})', { n: String(index) })}
    </Typography>
  );
}

/**
 * The answer's Local work turn ran a command outside the sandbox (ADR-0061 decision 5, #3069): Main stamps
 * `meta.local_work.ran_outside_sandbox` from the commit's trust record. A still label, never a colour alone.
 */
export function RanOutsideSandboxMarker() {
  return (
    <Typography variant="labelSmall" color="warning.main" data-testid="chat-message-ran-outside-sandbox" sx={{ display: 'block', mt: 0.5 }}>
      {t('features.chatMessages.ranOutsideSandbox', 'Ran outside the sandbox')}
    </Typography>
  );
}

/**
 * The answer's Local work turn ran in Full auto (ADR-0061 amendment of 2026-11-26, issue 3079): the agent answered
 * every approval, including those asked after it read outside content. Main stores `meta.local_work.full_auto`.
 */
export function FullAutoMarker() {
  return (
    <Typography variant="labelSmall" color="error.main" data-testid="chat-message-full-auto" sx={{ display: 'block', mt: 0.5 }}>
      {t('features.chatMessages.fullAuto', 'Ran in Full auto')}
    </Typography>
  );
}

/**
 * Whether the answer's Local work turn applied its folder's project instructions (issue 3130): the desktop thread's
 * "Respect AGENTS.md" was on or off. Read-only here: the switch is the desktop's. Main stores
 * `meta.local_work.project_instructions`; the files are the label's title.
 */
export function ProjectInstructionsMarker({ applied, files }: { readonly applied: boolean; readonly files: readonly string[] }) {
  return (
    <Typography variant="labelSmall" color="text.secondary" data-testid="chat-message-project-instructions" data-applied={String(applied)} title={files.join(', ')} sx={{ display: 'block', mt: 0.5 }}>
      {applied ? t('features.chatMessages.agentsMdApplied', 'AGENTS.md applied') : t('features.chatMessages.agentsMdIgnored', 'AGENTS.md ignored')}
    </Typography>
  );
}

/**
 * An answer's run marks, in order: continued automatically, outside the sandbox, Full auto, project instructions. The
 * desktop states project instructions once, in the thread's Participants tab (issue 3130), never under a message.
 */
export function AnswerMarkers({ answer }: { readonly answer: Pick<ChatMessage, 'continuedAutomatically' | 'continuationIndex' | 'ranOutsideSandbox' | 'fullAuto' | 'projectInstructions'> }) {
  return (
    <>
      {answer.continuedAutomatically === true && <ContinuedAutomaticallyLabel index={answer.continuationIndex} />}
      {answer.ranOutsideSandbox === true && <RanOutsideSandboxMarker />}
      {answer.fullAuto === true && <FullAutoMarker />}
      {answer.projectInstructions !== undefined && import.meta.env.MODE !== 'desktop' && <ProjectInstructionsMarker {...answer.projectInstructions} />}
    </>
  );
}

/** The answer stopped at its step limit: under a grant that continues the run, a continuation follows. */
export function StepLimitNotice() {
  return (
    <Alert severity="info" data-testid="chat-message-step-limit" sx={{ alignSelf: 'stretch', width: '100%' }}>
      {t('features.chatMessages.stepLimitReached', 'The run stopped at its step limit. When the unattended grant allows it, the run continues automatically in a new turn.')}
    </Alert>
  );
}
