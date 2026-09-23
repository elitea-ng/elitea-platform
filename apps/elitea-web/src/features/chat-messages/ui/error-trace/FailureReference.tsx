import type { ReactNode } from 'react';
import { Box, Button, Typography } from '@mui/material';
import { t } from '@/shared/i18n';
import { handleCopy } from '@/shared/lib/clipboard';

interface FailureReferenceProps {
  readonly messageId: string;
  readonly code: string | undefined;
}

/** Copy public identifiers without copying conversation content or internal traces. */
export function FailureReference({ messageId, code }: FailureReferenceProps): ReactNode {
  const reference = [code ? `Error code: ${code}` : undefined, `Message ID: ${messageId}`]
    .filter(Boolean).join('\n');
  return (
    <Box component="details" sx={{ mt: 1 }} data-testid="failure-reference">
      <Typography component="summary" variant="body2" sx={{ cursor: 'pointer' }}>
        {t('chatMessages.error.supportDetails', 'Details for support')}
      </Typography>
      <Typography component="pre" variant="caption" sx={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', my: 1 }}>
        {reference}
      </Typography>
      <Button size="small" onClick={() => { void handleCopy(reference); }}>
        {t('chatMessages.error.copyReference', 'Copy error reference')}
      </Button>
    </Box>
  );
}
