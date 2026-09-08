import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

interface BucketFooterProps {
  readonly bucketCount: number;
  readonly totalSize: string;
}

/**
 * "Buckets: N   Size: X" — the panel's bottom rule.
 *
 * Ported from `apps/elitea-ui/src/pages/Artifacts/Components/BucketFooter.jsx`.
 * Measured on next.elitea.ai: a 45px strip, `0.75rem 1.5rem`, `background.
 * tabPanel` under a divider rule, labels at `bodySmall2` in `text.primary` and
 * their values in `text.secondary`.
 */
export function BucketFooter(props: BucketFooterProps): ReactNode {
  return (
    <Box sx={footerSx}>
      <Box sx={statSx}>
        <Typography
          component="span"
          variant="bodySmall2"
          color="text.primary"
        >
          {t('artifacts.buckets.countLabel', 'Buckets:')}
        </Typography>
        <Typography
          component="span"
          variant="bodySmall2"
          sx={valueSx}
        >
          {props.bucketCount}
        </Typography>
      </Box>
      <Box sx={statSx}>
        <Typography
          component="span"
          variant="bodySmall2"
          color="text.primary"
        >
          {t('artifacts.buckets.sizeLabel', 'Size:')}
        </Typography>
        <Typography
          component="span"
          variant="bodySmall2"
          sx={valueSx}
        >
          {props.totalSize}
        </Typography>
      </Box>
    </Box>
  );
}

const footerSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'flex-start',
  flexShrink: 0,
  gap: theme.spacing(2),
  padding: theme.spacing(1.5, 3),
  marginTop: 'auto',
  borderTop: `0.0625rem solid ${theme.vars.palette.divider}`,
  backgroundColor: theme.vars.palette.background.tabPanel,
});
// See `ArtifactGridRow.tsx`'s `cellTextSx`.
const valueSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.secondary });
const statSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.5),
});
