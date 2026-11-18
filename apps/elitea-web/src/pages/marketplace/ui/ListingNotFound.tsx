/**
 * A listing id that names nothing: the listing was unpublished, or the link is
 * wrong. One clear way out — back to the Marketplace.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';

import { Link } from '@tanstack/react-router';

import { t } from '@/shared/i18n';

export function ListingNotFound(): React.JSX.Element {
  return (
    <Box
      data-testid="marketplace-listing-not-found"
      sx={{ padding: '1.5rem', display: 'flex', flexDirection: 'column', gap: '1rem', alignItems: 'flex-start' }}
    >
      <Alert severity="warning">
        {t('marketplace.listing.notFound', 'This listing is not in the Marketplace any more.')}
      </Alert>
      <Button component={Link} to="/marketplace" variant="outlined">
        {t('marketplace.listing.back', 'Back to the Marketplace')}
      </Button>
    </Box>
  );
}
