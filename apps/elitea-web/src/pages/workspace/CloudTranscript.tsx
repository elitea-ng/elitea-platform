/**
 * The thread's transcript while the cloud runs it: the conversation's own
 * messages on the server, rendered by the chat's own message list (so tool
 * steps, errors with their support details and the streamed reply look as in
 * the chat), with a live cloud turn folded in as it streams (`useCloudChat`).
 * The earlier local turns' tool rows come back with the take back.
 *
 * A cloud turn that waits on the person is never silent here: the chat's own
 * approval / continue cards answer inline; a pause only the chat can answer
 * shows an "Answer in chat" card.
 */
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { ChatMessageList } from '@/features/chat-messages';
import { t } from '@/shared/i18n';

import type { CloudChat } from './useCloudChat';

/** A pause only the web chat can answer (several approvals at once, an authored pause, node recovery): never silent, one click away. */
function AnswerInChatCard({ chat }: { chat: CloudChat }): React.JSX.Element {
  return (
    <Box
      component="output"
      data-testid="cloud-answer-in-chat"
      sx={(theme: Theme) => ({
        display: 'flex',
        alignItems: 'center',
        gap: 1.5,
        padding: 1.25,
        borderRadius: '10px',
        background: `color-mix(in srgb, ${theme.vars.palette.warning.main} 12%, transparent)`,
        boxShadow: `inset 0 0 0 0.5px color-mix(in srgb, ${theme.vars.palette.warning.main} 45%, transparent)`,
      })}
    >
      <Typography variant="bodySmall" sx={{ flex: 1 }}>
        {t('workspace.cloud.waiting', 'The cloud copy is waiting for an answer that only the chat can take.')}
      </Typography>
      <Button size="small" variant="outlined" onClick={chat.openInChat}>
        {t('workspace.cloud.answerInChat', 'Answer in chat')}
      </Button>
    </Box>
  );
}

export function CloudTranscript({ chat }: { chat: CloudChat }): React.JSX.Element {
  if (chat.loading && chat.history.length === 0) {
    return (
      <Typography variant="bodySmall" component="output" sx={{ alignSelf: 'center' }}>
        {t('workspace.history.loading', 'Loading earlier messages')}
      </Typography>
    );
  }
  return (
    <div data-testid="cloud-transcript" style={{ display: 'contents' }}>
      <ChatMessageList
        chatHistory={chat.history}
        isStreaming={chat.streaming}
        {...(chat.userId !== undefined ? { userId: chat.userId } : {})}
        projectId={chat.projectId}
        // The chat's own approval and "continue" cards, answered through the chat's own resume route; what
        // only the chat can answer (several cards at once, a static pause, MCP authorization) goes there in one click.
        continuation={{
          onHitlResume: (decision) => void chat.answerHitl(decision),
          onContinueTokenLimitExecution: (messageId) => void chat.continueTurn(messageId),
          onContinueMcpExecution: () => chat.openInChat(),
          hideHitlActions: chat.prompt?.kind === 'chat',
        }}
      />
      {chat.prompt?.kind === 'chat' && <AnswerInChatCard chat={chat} />}
    </div>
  );
}
