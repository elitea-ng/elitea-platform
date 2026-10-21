/**
 * The chat page's half of the conversation fork (client contract 1.11,
 * `forkConversation`), kept out of `index.tsx` for its file-length budget:
 *
 * - `useChatFork`: "Fork from here" on a message forks the open conversation
 *   up to and including that message and opens the new conversation;
 * - `ForkedFromLink`: the small "Forked from <name>" line above a fork's
 *   transcript, which opens the source. It renders only when the server
 *   named the source (`forked_from.name`), that is, while the reader can
 *   still see it.
 */
import { useCallback, useState } from 'react';

import { useNavigate } from '@tanstack/react-router';

import CallSplitOutlinedIcon from '@mui/icons-material/CallSplitOutlined';
import Box from '@mui/material/Box';
import Link from '@mui/material/Link';
import Snackbar from '@mui/material/Snackbar';
import Typography from '@mui/material/Typography';

import { useForkConversation } from '@/features/chat-conversation-list';
import { t } from '@/shared/i18n';

import type { ForkedFromLinkData } from './forkedFrom';

export interface ChatFork {
  /** Absent while there is no open, stored conversation to fork. */
  readonly onForkFromMessage: ((messageId: string) => void) | undefined;
  /** The last failure, told in a snackbar (`ForkErrorSnackbar`). */
  readonly error: string | undefined;
  readonly dismissError: () => void;
}

export function useChatFork(projectId: string | undefined, conversationId: string | undefined): ChatFork {
  const navigate = useNavigate();
  const [error, setError] = useState<string | undefined>(undefined);
  const fork = useForkConversation({ projectId, toastError: setError });
  const onForkFromMessage = useCallback(
    (messageId: string) => {
      if (conversationId === undefined) return;
      void fork(conversationId, { messageId }).then((made) => {
        if (made !== undefined) void navigate({ to: '/chat/$conversationId', params: { conversationId: made.id } });
      });
    },
    [conversationId, fork, navigate],
  );
  const dismissError = useCallback(() => setError(undefined), []);
  return { onForkFromMessage: conversationId === undefined ? undefined : onForkFromMessage, error, dismissError };
}

export function ForkErrorSnackbar({ fork }: { readonly fork: ChatFork }): React.JSX.Element {
  return <Snackbar open={fork.error !== undefined} message={fork.error} autoHideDuration={6000} onClose={fork.dismissError} />;
}

export function ForkedFromLink({ forkedFrom }: { readonly forkedFrom: ForkedFromLinkData | undefined }): React.JSX.Element | null {
  const navigate = useNavigate();
  if (forkedFrom === undefined) return null;
  return (
    <Box data-testid="chat-forked-from" sx={{ display: 'flex', alignItems: 'center', gap: 0.5, px: 2, pt: 1, color: 'text.secondary' }}>
      <CallSplitOutlinedIcon fontSize="small" />
      <Typography variant="bodySmall" component="span">
        {t('pages.chat.fork.forkedFrom', 'Forked from')}{' '}
        <Link
          component="button"
          type="button"
          variant="bodySmall"
          onClick={() => void navigate({ to: '/chat/$conversationId', params: { conversationId: forkedFrom.conversationId } })}
        >
          {forkedFrom.name === '' ? t('pages.chat.fork.untitled', 'an untitled chat') : forkedFrom.name}
        </Link>
      </Typography>
    </Box>
  );
}
