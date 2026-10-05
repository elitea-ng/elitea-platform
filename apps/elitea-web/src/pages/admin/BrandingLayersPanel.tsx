/**
 * Admin › Branding — which layers contribute, and which one decides each
 * field (ADR-0024 WP4).
 *
 * The served pack is `product default ← mounted file ← database`, merged
 * field by field. `layers` says whether the two operator-controlled layers
 * contribute at all; the per-field column says which layer a field's
 * EFFECTIVE value comes from, derived from the stored values (a stored value
 * is the database's; an inherited one is the file's when a file contributes,
 * else the product default's).
 */
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { sourceLabel } from './BrandingField';
import {
  BRANDING_KEYS,
  brandingFieldSource,
  type BrandingLayers,
  type BrandingValues,
} from './brandingValues';

export interface BrandingLayersPanelProps {
  readonly layers: BrandingLayers;
  readonly values: BrandingValues;
}

function LayerChip({ label, contributes }: { readonly label: string; readonly contributes: boolean }) {
  return (
    <Chip
      size="small"
      variant={contributes ? 'filled' : 'outlined'}
      color={contributes ? 'primary' : 'default'}
      label={`${label}: ${
        contributes
          ? t('pages.admin.branding.layers.contributes', 'contributes')
          : t('pages.admin.branding.layers.absent', 'absent')
      }`}
    />
  );
}

export function BrandingLayersPanel({ layers, values }: BrandingLayersPanelProps) {
  return (
    <Box component="section" aria-labelledby="branding-layers-heading" data-testid="branding-layers" sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      <Typography id="branding-layers-heading" variant="headingSmall" component="h2">
        {t('pages.admin.branding.layers.title', 'Layers')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.branding.layers.description',
          'The served brand is the product default, under the mounted file pack, under what is saved here. Each field takes the topmost layer that sets it.',
        )}
      </Typography>
      <Box sx={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap' }}>
        <LayerChip label={t('pages.admin.branding.layers.file', 'Mounted file pack')} contributes={layers.file} />
        <LayerChip label={t('pages.admin.branding.layers.database', 'Database')} contributes={layers.database} />
      </Box>
      <Table size="small" aria-label={t('pages.admin.branding.layers.table', 'Field sources')}>
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.branding.layers.column.field', 'Field')}</TableCell>
            <TableCell>{t('pages.admin.branding.layers.column.source', 'Decided by')}</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {BRANDING_KEYS.map((key) => (
            <TableRow key={key} data-testid={`branding-layer-row-${key}`}>
              <TableCell component="th" scope="row" sx={{ fontFamily: (theme) => theme.typography.fontFamily }}>
                {key}
              </TableCell>
              <TableCell>{sourceLabel(brandingFieldSource(values, key, layers))}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Box>
  );
}
