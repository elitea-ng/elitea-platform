/**
 * The thread's transcript while the cloud runs it: the conversation's own
 * messages on the server, read again every few seconds (the cloud's turns
 * are not on this computer). It is the server fallback the earlier-turns
 * view already uses, kept live. Tool rows of the thread's earlier local turns
 * come back with the take back.
 */
import { useQuery } from '@tanstack/react-query';

import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { ServerTurn } from './sessionParts';
import { serverMessages } from './useThreadHistory';

const CLOUD_TRANSCRIPT_POLL_MS = 3000;

export function CloudTranscript({ projectId, conversationId }: { projectId: number; conversationId: string }): React.JSX.Element {
  const messages = useQuery({
    queryKey: ['workspace', 'cloud-transcript', projectId, conversationId],
    queryFn: () => serverMessages(projectId, conversationId),
    refetchInterval: CLOUD_TRANSCRIPT_POLL_MS,
    refetchIntervalInBackground: false,
    retry: false,
  });
  if (messages.data === undefined) {
    return (
      <Typography variant="bodySmall" component="output" sx={{ alignSelf: 'center' }}>
        {messages.isError ? t('workspace.handoff.transcriptFailed', 'The cloud’s messages could not be read. Trying again…') : t('workspace.history.loading', 'Loading earlier messages')}
      </Typography>
    );
  }
  return (
    <div data-testid="cloud-transcript" style={{ display: 'contents' }}>
      {messages.data.map((message) => (
        <ServerTurn key={message.id} message={message} />
      ))}
    </div>
  );
}
