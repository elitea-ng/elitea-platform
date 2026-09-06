import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { UnavailableIcon } from '@/shared/ui/icons/unavailable-icon';

interface ArtifactTableEmptyProps {
  readonly onUpload: () => void;
}

/**
 * The bucket-with-no-files state. Ported from `ArtifactTableNoFiles.jsx`: it
 * REPLACES the header and the rows rather than sitting under an empty header
 * strip, which is what the baseline does and what keeps the message centred in
 * the whole panel.
 */
export function ArtifactTableEmpty(props: ArtifactTableEmptyProps): ReactNode {
  return (
    <Box sx={wrapperSx}>
      <Box sx={contentSx}>
        <UnavailableIcon style={iconStyle} />
        <Typography
          variant="bodyMedium"
          sx={messageSx}
        >
          {t('artifacts.table.empty', 'No files in this bucket')}
        </Typography>
        <BaseBtn
          variant="elitea"
          color="secondary"
          onClick={props.onUpload}
        >
          {t('artifacts.table.uploadFiles', 'Upload files')}
        </BaseBtn>
      </Box>
    </Box>
  );
}

const iconStyle = { width: '2rem', height: '2rem' };
const messageSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.secondary });
const wrapperSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  justifyContent: 'center',
  flex: 1,
  padding: theme.spacing(6),
  textAlign: 'center',
});
const contentSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  gap: theme.spacing(2),
});
