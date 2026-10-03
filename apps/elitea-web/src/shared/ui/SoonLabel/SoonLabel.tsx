import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { combineSx } from '../lib/combineSx';
import { t } from '@/shared/i18n';
import { typeScale } from '@/shared/brand/typography';

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface SoonLabelProps {
  /** Primary label text, rendered to the left of the "Soon" chip. */
  text: ReactNode;
  sx?: SxProps<Theme>;
}

/**
 * A row label with a trailing "Soon" pill, used to mark not-yet-available
 * menu entries and feature flags. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/soon-label/SoonLabel.jsx`.
 */
export function SoonLabel({ text, sx }: SoonLabelProps): ReactNode {
  return (
    <Box
      sx={combineSx(
        {
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          width: '100%',
          gap: (theme: Theme) => theme.spacing(1),
        },
        sx,
      )}
    >
      <Box component="span">{text}</Box>
      <Box
        component="span"
        sx={(theme) => ({
          display: 'inline-flex',
          alignItems: 'center',
          flexShrink: 0,
          paddingBlock: theme.spacing(0.125),
          paddingInline: theme.spacing(1),
          borderRadius: theme.vars.shape.radiusLg,
          border: `1px solid ${theme.vars.palette.border.lines}`,
          color: theme.vars.palette.text.secondary,
          ...typeScale(theme.typography.labelSmall),
        })}
      >
        {t('shared.ui.soonLabel.soon', 'Soon')}
      </Box>
    </Box>
  );
}
