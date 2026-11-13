/**
 * Tells the person why removing a participant did not happen (#2014).
 *
 * The rail's remove action was fire-and-forget: a refusal (the conversation's
 * owner cannot be removed) left the row where it was and said nothing, which
 * reads as a broken button. The server explains the refusal in its flat
 * `{"error": "..."}` body; that sentence is shown, with a plain fallback for a
 * failure it did not explain.
 */
import { useCallback, useState } from 'react';

import Alert from '@mui/material/Alert';
import Snackbar from '@mui/material/Snackbar';

import { EliteaApiError } from '@/shared/api/generated/mutator';
import { serverErrorSentence } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';

export interface ParticipantRemoveError {
  readonly message: string | undefined;
  readonly report: (error: unknown) => void;
  readonly dismiss: () => void;
}

export function useParticipantRemoveError(): ParticipantRemoveError {
  const [message, setMessage] = useState<string | undefined>(undefined);
  const report = useCallback((error: unknown) => {
    const fallback = t('pages.chat.participants.removeFailed', 'The participant could not be removed from the conversation.');
    const explained = error instanceof EliteaApiError && error.failure.kind === 'http' ? serverErrorSentence(error.failure.body) : undefined;
    setMessage(explained ?? fallback);
  }, []);
  const dismiss = useCallback(() => setMessage(undefined), []);
  return { message, report, dismiss };
}

export function ParticipantRemoveErrorSnackbar({ state }: { readonly state: ParticipantRemoveError }): React.JSX.Element {
  return (
    <Snackbar open={state.message !== undefined} autoHideDuration={6000} onClose={state.dismiss} anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}>
      <Alert severity="error" variant="filled" onClose={state.dismiss}>
        {state.message}
      </Alert>
    </Snackbar>
  );
}
