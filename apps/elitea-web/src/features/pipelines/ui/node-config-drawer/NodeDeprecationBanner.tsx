import type { ReactNode } from 'react';
import { memo } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { TextWithLink } from '@/shared/ui/TextWithLink';

import { DeprecatedTips } from '../../lib/flow-editor/constants/deprecated.constants';

export interface NodeDeprecationBannerProps {
  readonly type: string;
}

const contentSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
  minWidth: 0,
};

/**
 * Warning banner displayed inside `NodeConfigPanel` for deprecated/refused node types.
 */
export const NodeDeprecationBanner = memo(function NodeDeprecationBanner({
  type,
}: NodeDeprecationBannerProps): ReactNode {
  const tip = Object.hasOwn(DeprecatedTips, type) ? DeprecatedTips[type] : undefined;
  if (!tip) return null;

  return (
    <Alert
      severity="warning"
      variant="outlined"
      data-testid="pipeline-node-panel-deprecation-banner"
    >
      <Box sx={contentSx}>
        <Typography variant="labelSmall" component="div">
          <TextWithLink {...tip} />
        </Typography>
        <Typography
          variant="bodySmall"
          data-testid="pipeline-node-panel-deprecation-read-only"
        >
          {t(
            'pipelines.nodeConfigPanel.deprecatedReadOnlyHint',
            'Deprecated nodes are refused when saving and are read-only in Flow view. Edit in YAML view or replace with a supported node.',
          )}
        </Typography>
      </Box>
    </Alert>
  );
});
