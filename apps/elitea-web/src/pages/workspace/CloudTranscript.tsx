/**
 * The thread's transcript while the cloud runs it, drawn exactly like the
 * local one: your prompts, then what the agent did — its text and its tool
 * steps — through the same components (`YourPrompt`, `TurnTranscript`), with
 * the conversation's own messages from the server and a live cloud turn folded
 * in as it streams (`useCloudChat`). No avatars, reactions or timestamps: the
 * local transcript has none either.
 *
 * A cloud turn that waits on the person is never silent here: an approval, or
 * the output-limit "continue", is an inline card in the local style and is
 * answered through the chat's resume route; a pause only the chat can answer
 * shows an "Answer in chat" card.
 */
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { approval, TurnTranscript } from '@/features/workspace';
import { t } from '@/shared/i18n';

import type { Card } from './cloudPrompts';
import { exchangesOf, turnViewOf } from './cloudTranscriptModel';
import { useCopyMenu, YourPrompt } from './sessionParts';
import type { CloudChat } from './useCloudChat';

function ApprovalPrompt({ chat, card }: { chat: CloudChat; card: Card | undefined }): React.JSX.Element {
  const decide = (action: 'approve' | 'reject') => () => void chat.answerHitl({ action, toolCallId: card?.tool_call_id, interruptId: card?.interrupt_id });
  const args = card?.tool_args;
  return (
    <approval.Card
      title={card?.action_label ?? card?.tool_name ?? t('workspace.cloud.approvalTitle', 'The cloud copy asks to continue')}
      detail={card?.policy_message ?? card?.message}
      params={args !== null && typeof args === 'object' ? (args as Record<string, unknown>) : undefined}
      approveLabel={t('workspace.cloud.approve', 'Approve')}
      onApprove={decide('approve')}
      denyLabel={t('workspace.cloud.deny', 'Deny')}
      onDeny={decide('reject')}
      testId="cloud-approval-card"
    />
  );
}

function PromptCard({ chat }: { chat: CloudChat }): React.JSX.Element | null {
  const { prompt } = chat;
  if (prompt === null) return null;
  if (prompt.kind === 'approval') return <ApprovalPrompt chat={chat} card={prompt.cards[0]} />;
  if (prompt.kind === 'continue') {
    return (
      <approval.Card
        title={t('workspace.cloud.continueTitle', 'The cloud copy stopped at its output limit')}
        detail={prompt.message.requiresConfirmation?.message}
        approveLabel={t('workspace.cloud.continueButton', 'Continue')}
        onApprove={() => void chat.continueTurn(prompt.message.id)}
        testId="cloud-continue-card"
      />
    );
  }
  return (
    <approval.Card
      title={t('workspace.cloud.waiting', 'The cloud copy is waiting for an answer that only the chat can take.')}
      approveLabel={t('workspace.cloud.answerInChat', 'Answer in chat')}
      onApprove={chat.openInChat}
      testId="cloud-answer-in-chat"
    />
  );
}

export function CloudTranscript({ chat }: { chat: CloudChat }): React.JSX.Element {
  const copyMenu = useCopyMenu();
  if (chat.loading && chat.history.length === 0) {
    return (
      <Typography variant="bodySmall" component="output" sx={{ alignSelf: 'center' }}>
        {t('workspace.history.loading', 'Loading earlier messages')}
      </Typography>
    );
  }
  return (
    <div data-testid="cloud-transcript" style={{ display: 'contents' }}>
      {exchangesOf(chat.history).map((exchange) => (
        <Box key={exchange.key} sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
          {exchange.prompt !== undefined && <YourPrompt text={exchange.prompt.content} />}
          {exchange.answers.map((answer) => (
            <TurnTranscript key={answer.id} view={turnViewOf(answer)} busy={chat.streaming && answer.isStreaming === true} onTextContextMenu={copyMenu} />
          ))}
        </Box>
      ))}
      <PromptCard chat={chat} />
    </div>
  );
}
