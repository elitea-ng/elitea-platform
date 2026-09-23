import { useState, type ReactNode } from 'react';
import { Alert, AlertTitle, Box, Button, Collapse, Typography } from '@mui/material';
import { t } from '@/shared/i18n';
import { handleCopy } from '@/shared/lib/clipboard';

interface ContinuationErrorProps {
  readonly error: unknown;
  readonly partialOutput: string;
  readonly children: ReactNode;
}

/** Incomplete output is inspectable without presenting it as a completed answer. */
export function ContinuationError({ error, partialOutput, children }: ContinuationErrorProps): ReactNode {
  const [expanded, setExpanded] = useState(false);
  return (
    <Box data-testid="continuation-error">
      <Alert severity="warning">
        <AlertTitle>{t('chatMessages.continuation.incomplete', 'The model response is incomplete')}</AlertTitle>
        {typeof error === 'string' ? error : t('chatMessages.continuation.failed', 'Automatic continuation could not finish the response.')}
        <Typography variant="body2" sx={{ mt: 1 }}>
          {t('chatMessages.continuation.guidance', 'Increase the output limit in Model settings, or ask for a smaller part of the task, then try again.')}
        </Typography>
      </Alert>
      {children != null && (
        <Box sx={{ mt: 1 }}>
          <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1 }}>
            <Button size="small" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)}>
              {expanded
                ? t('chatMessages.continuation.hidePartial', 'Hide partial response')
                : t('chatMessages.continuation.showPartial', 'Show partial response')}
            </Button>
            {partialOutput !== '' && <Button size="small" onClick={() => { void handleCopy(partialOutput); }}>
              {t('chatMessages.continuation.copyPartial', 'Copy partial response')}
            </Button>}
          </Box>
          <Collapse in={expanded} unmountOnExit>
            <Box sx={{ maxHeight: '32rem', overflow: 'auto', mt: 1 }}>{children}</Box>
          </Collapse>
        </Box>
      )}
    </Box>
  );
}
