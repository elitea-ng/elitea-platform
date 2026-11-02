/**
 * The mount point of `features/chat-messages`' `PlaybackChatBox`.
 *
 * **Why `processes/chat/ui/`** — the same reason `ChatWithEditors` lives here: this needs `features/chat-messages` AND
 * `widgets/app-shell`'s selected project at once, and `.dependency-
 * cruiser.cjs` puts `processes/` above both.
 *
 * **Why the URL and not component state.** Playback is entered from a row
 * of the sidebar's chat list (`widgets/chat-rail`), outside this page;
 * routing the signal through `?playback=1` on the conversation the user is
 * already navigating to keeps the two columns from having to share a store,
 * and makes the surface linkable, reloadable and testable. A playback view
 * reachable only from in-memory state is a view nothing can reach twice.
 *
 * The transcript itself comes from the conversation-details endpoint, whose
 * `chat_history` is exactly what `PlaybackChatBox` seeds its history from;
 * further pages are the box's own `useLoadPlaybackMessages` concern.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import LinearProgress from '@mui/material/LinearProgress';
import Snackbar from '@mui/material/Snackbar';
import Typography from '@mui/material/Typography';

import { conversationApi } from '@/entities/conversation';
import { PlaybackChatBox } from '@/features/chat-messages';
import { t } from '@/shared/i18n';
import { useSelectedProject } from '@/widgets/app-shell';

export interface ChatPlaybackProps {
  /** The conversation to replay — `/chat/$conversationId`'s own param. */
  readonly conversationId: string;
}

/** @public Rendered by `ChatWithEditors` in place of the live chat when `?playback=1`. */
export function ChatPlayback({ conversationId }: ChatPlaybackProps): ReactNode {
  const { project } = useSelectedProject();
  const projectId = project?.id;
  const [errorMessage, setErrorMessage] = useState<string | undefined>(undefined);

  const details = conversationApi.useDetails(
    { projectId: projectId ?? '', id: conversationId },
    { enabled: projectId !== undefined },
  );

  if (details.isPending) {
    return (
      <Box
        data-testid="chat-playback"
        sx={{ height: '100%', p: 2 }}
      >
        <LinearProgress />
      </Box>
    );
  }

  if (details.isError || details.data === undefined) {
    return (
      <Box
        data-testid="chat-playback"
        sx={{ height: '100%', p: 2 }}
      >
        <Alert severity="error">
          {t('processes.chat.playback.loadError', 'This conversation could not be loaded for playback.')}
        </Alert>
      </Box>
    );
  }

  return (
    <Box
      data-testid="chat-playback"
      sx={{ height: '100%', minHeight: 0, display: 'flex', flexDirection: 'column', p: 2 }}
    >
      <Typography variant="headingSmall">{details.data.name}</Typography>
      <PlaybackChatBox
        conversation={details.data}
        {...(projectId !== undefined ? { projectId } : {})}
        toastError={setErrorMessage}
      />
      {/* A local Snackbar: this app has no global toast host. */}
      <Snackbar
        open={errorMessage !== undefined}
        autoHideDuration={6000}
        onClose={() => setErrorMessage(undefined)}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
      >
        <Alert
          severity="error"
          onClose={() => setErrorMessage(undefined)}
          variant="filled"
        >
          {errorMessage}
        </Alert>
      </Snackbar>
    </Box>
  );
}
