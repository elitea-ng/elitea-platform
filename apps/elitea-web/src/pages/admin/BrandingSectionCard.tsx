/**
 * What the Configuration page shows for the `branding` section in place of
 * the generic form (ADR-0024 WP4): a short card that points at the Branding
 * page, which edits the same rows as a brand pack. See `Configuration.tsx`'s
 * `SECTION_ID_EDITORS` for why the section is keyed on its id.
 */
import { Link } from '@tanstack/react-router';

import PaletteOutlinedIcon from '@mui/icons-material/PaletteOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

export function BrandingSectionCard() {
  return (
    <Box
      data-testid="admin-configuration-branding-card"
      sx={(theme) => ({
        display: 'flex',
        flexDirection: 'column',
        gap: '0.75rem',
        padding: '1rem',
        border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        borderRadius: theme.vars.shape.radiusMd,
        maxWidth: '36rem',
      })}
    >
      <Typography variant="headingSmall" component="h2">
        {t('pages.admin.configuration.branding.title', 'Branding has a page of its own')}
      </Typography>
      <Typography variant="bodyMedium" color="text.secondary">
        {t(
          'pages.admin.configuration.branding.body',
          'The brand colour derives every accent, the logos and fonts are uploaded, and the result is previewed before it is saved. This section edits the same values, so it is not offered as a plain form here.',
        )}
      </Typography>
      <Box>
        <Button
          component={Link}
          to="/branding"
          size="small"
          variant="elitea"
          color="primary"
          startIcon={<PaletteOutlinedIcon />}
          data-testid="admin-configuration-branding-link"
        >
          {t('pages.admin.configuration.branding.open', 'Open Branding')}
        </Button>
      </Box>
    </Box>
  );
}
