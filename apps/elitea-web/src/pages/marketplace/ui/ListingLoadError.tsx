/**
 * A listing that failed to load for a reason other than "it is gone" (a network
 * error, a 5xx): an error with a Retry, not the not-found page that would tell
 * the reader the listing does not exist.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';

import { t } from '@/shared/i18n';

export function ListingLoadError({ onRetry }: { readonly onRetry: () => void }): ReactNode {
  return (
    <Alert
      severity="error"
      data-testid="marketplace-listing-error"
      action={
        <Button color="inherit" size="small" onClick={onRetry} data-testid="marketplace-listing-retry">
          {t('marketplace.listing.retry', 'Retry')}
        </Button>
      }
    >
      {t('marketplace.listing.loadError', 'The listing did not load. Try again.')}
    </Alert>
  );
}
