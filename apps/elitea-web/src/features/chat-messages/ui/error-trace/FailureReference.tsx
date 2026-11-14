import type { ReactNode } from 'react';
import { Box, Button, Typography } from '@mui/material';
import { t } from '@/shared/i18n';
import { handleCopy } from '@/shared/lib/clipboard';
import { WELCOME_MESSAGE_ID } from '@/shared/lib/enums';

interface FailureReferenceProps {
  readonly messageId: string;
  readonly code: string | undefined;
}

/** No code and no stored id (a client-side message): there is nothing a log lookup can use but the time. */
function NoReference(): ReactNode {
  return (
    <Box component="details" sx={{ mt: 1 }} data-testid="failure-reference">
      <Typography component="summary" variant="bodyMedium" sx={{ cursor: 'pointer' }}>
        {t('chatMessages.error.supportDetails', 'Details for support')}
      </Typography>
      <Typography variant="bodyMedium" component="p" color="text.secondary">
        {t('chatMessages.error.noReference', 'No error reference is available for this failure. Contact your administrator and say when it happened.')}
      </Typography>
    </Box>
  );
}

/** Copy public identifiers without copying conversation content or internal traces. */
export function FailureReference({ messageId, code }: FailureReferenceProps): ReactNode {
  // The welcome message is a client-side placeholder (it carries the index run
  // history's failure as well): its id names nothing a server log could find.
  const realMessageId = messageId === WELCOME_MESSAGE_ID ? undefined : messageId;
  const reference = [code ? `Error code: ${code}` : undefined, realMessageId ? `Message ID: ${realMessageId}` : undefined]
    .filter(Boolean).join('\n');
  if (!reference) return <NoReference />;
  return (
    <Box component="details" sx={{ mt: 1 }} data-testid="failure-reference">
      <Typography component="summary" variant="bodyMedium" sx={{ cursor: 'pointer' }}>
        {t('chatMessages.error.supportDetails', 'Details for support')}
      </Typography>
      <Typography component="pre" variant="bodySmall" sx={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', my: 1 }}>
        {reference}
      </Typography>
      <Typography variant="bodyMedium" component="p" color="text.secondary">
        {t('chatMessages.error.operatorDiagnostics', 'Share this reference with your administrator. Detailed diagnostics are available to operators in service logs.')}
      </Typography>
      <Button size="small" onClick={() => { void handleCopy(reference); }}>
        {t('chatMessages.error.copyReference', 'Copy error reference')}
      </Button>
    </Box>
  );
}
