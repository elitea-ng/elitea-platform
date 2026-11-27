import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Snackbar from '@mui/material/Snackbar';

/** Why a Chat from an Agents card did not start (issue 3133). */
export function AgentChatError({ message, onClose }: { readonly message: string | undefined; readonly onClose: () => void }): ReactNode {
  return (
    <Snackbar
      open={message !== undefined}
      autoHideDuration={8000}
      onClose={onClose}
    >
      <Alert
        severity="error"
        data-testid="agent-card-chat-error"
        onClose={onClose}
      >
        {message ?? ''}
      </Alert>
    </Snackbar>
  );
}
