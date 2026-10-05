/**
 * ui/CredentialNotFoundValue.tsx — the `CredentialsSelect` selected-value
 * display when the current `elitea_title` matches no loaded option. Ported
 * from
 * `apps/elitea-ui/src/[fsd]/features/credentials/ui/credentials-select/CredentialNotFoundValue.jsx`.
 * Manifest COPY-112.
 */
import type { ReactNode } from 'react';

import PersonIcon from '@mui/icons-material/Person';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { BriefcaseIcon } from '@/shared/ui/icons/briefcase-icon';

export interface CredentialNotFoundValueProps {
  readonly eliteaTitle: string;
  readonly isPrivate?: boolean;
  readonly hasFetchedData: boolean;
}

/**
 * #6632: the value keeps the normal text colour, and the field carries no
 * attention icon. Both used to mark the mismatch here (a red value and an
 * orange icon), on top of the message `CredentialMismatchFooter` already
 * shows under the field. The footer is now the one place that says so.
 * `hasFetchedData` stays: before the list loads, the title is greyed out as
 * "not yet confirmed", not as "missing".
 */
export function CredentialNotFoundValue({ eliteaTitle, isPrivate, hasFetchedData }: CredentialNotFoundValueProps): ReactNode {
  return (
    <Box
      sx={containerSx}
      data-testid="credential-not-found-value"
    >
      {isPrivate ? <PersonIcon fontSize="inherit" /> : <BriefcaseIcon />}
      <Typography
        variant="labelMedium"
        sx={textSx(hasFetchedData)}
      >
        {eliteaTitle}
      </Typography>
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme: Theme) => ({
  flex: 1,
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1),
  color: theme.vars.palette.text.secondary,
});

const textSx = (loaded: boolean): SxProps<Theme> => (theme: Theme) => ({
  flex: 1,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
  color: loaded ? theme.vars.palette.text.secondary : theme.vars.palette.text.disabled,
});
