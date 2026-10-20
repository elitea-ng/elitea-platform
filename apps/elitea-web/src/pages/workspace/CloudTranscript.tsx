/**
 * The thread's transcript while the cloud runs it: the conversation's own
 * messages on the server, rendered by the chat's own message list (so tool
 * steps, errors with their support details and the streamed reply look as in
 * the chat), with a live cloud turn folded in as it streams (`useCloudChat`).
 * The earlier local turns' tool rows come back with the take back.
 */
import Typography from '@mui/material/Typography';

import { ChatMessageList } from '@/features/chat-messages';
import { t } from '@/shared/i18n';

import type { CloudChat } from './useCloudChat';

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
        // A cloud turn's approvals and "continue" prompts are answered on the web for now.
        continuation={{ hideContinueButton: true, hideHitlActions: true }}
      />
    </div>
  );
}
