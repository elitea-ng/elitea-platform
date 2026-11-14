import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { AttentionIcon } from '@/shared/ui/icons/attention-icon';
import { StopIcon } from '@/shared/ui/icons/stop-icon';
import { InfoTooltip } from '@/shared/ui/InfoTooltip';

import { IndexStatuses } from '../../lib/constants/indexDetails.constants';
import { toDisplayString } from '../../lib/helpers/displayString.local';
import { describeIndexError } from '../../lib/helpers/indexError.helpers';
import type { IndexRow } from '../../model/indexesStore';

/**
 * Port of `apps/elitea-ui/src/[fsd]/features/toolkits/indexes/ui/
 * IndexDetails/IndexNameWrapper.jsx` (unit A4a). Shows the index name plus
 * a status badge (error/partial/stopped) or an in-progress spinner.
 *
 * DISCLOSED DEVIATION: the baseline's failed-state badge used `InfoTooltip`
 * with a custom `icon` override (a differently-coloured "i" glyph via
 * `infoTooltip={{icon: styles.errorInfoIcon}}`). `shared/ui`'s
 * `InfoTooltip` (unit S1) deliberately dropped the icon-override capability
 * ("the redundant second path" — see its own doc comment); its `sx` prop
 * still reaches the icon, so the red colouring is applied that way instead
 * — same visual result, no icon override needed.
 */
export interface IndexNameWrapperProps {
  readonly index: IndexRow | null | undefined;
}

const nameWrapperSx: SxProps<Theme> = { display: 'flex', justifyContent: 'flex-start', alignItems: 'center', gap: '1rem' };
const progressWrapperSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  justifyContent: 'center',
  alignItems: 'center',
  padding: '.25rem .75rem',
  borderRadius: theme.vars.shape.radiusLg,
  gap: '0.5rem',
});

export function IndexNameWrapper(props: IndexNameWrapperProps): ReactNode {
  const { index } = props;

  const state = index?.metadata['state'];
  const isFailed = state === IndexStatuses.fail;
  const isPartlyOk = state === IndexStatuses.partlyOk;
  const isWarningBadge = state === IndexStatuses.fail || state === IndexStatuses.cancelled || state === IndexStatuses.partlyOk;

  // The failure's own reason (#2042), not only "Index processing error".
  const failureReason = isFailed ? describeIndexError(index?.metadata['error']) : undefined;

  const badgeLabel = useMemo(() => {
    if (isFailed) return failureReason ?? 'Index processing error';
    if (isPartlyOk) return 'Partially indexed';
    return 'Stopped';
  }, [isFailed, isPartlyOk, failureReason]);

  return (
    <Box sx={nameWrapperSx}>
      <Typography
        variant="headingSmall"
        color="text.secondary"
      >
        {toDisplayString(index?.metadata['collection'])}
      </Typography>
      {isWarningBadge && (
        <Box
          sx={{
            display: 'flex',
            justifyContent: 'center',
            alignItems: 'center',
            padding: '.25rem .75rem .25rem 0.25rem',
            borderRadius: (theme) => theme.vars.shape.radiusLg,
            border: (theme) => `1px solid ${isFailed ? theme.vars.palette.error.main : theme.vars.palette.warning.main}`,
            gap: '0.5rem',
          }}
        >
          {isFailed ? (
            <InfoTooltip
              title={failureReason ?? t('features.toolkits.indexNameWrapper.processingError', 'Index processing error')}
              disableTooltip
              sx={{ color: 'error.main' }}
            />
          ) : isPartlyOk ? (
            <AttentionIcon
              width={16}
              height={16}
            />
          ) : (
            <StopIcon
              width={16}
              height={16}
            />
          )}
          <Typography
            variant="bodySmall"
            color="text.secondary"
          >
            {badgeLabel}
          </Typography>
        </Box>
      )}
      {state === IndexStatuses.progress && (
        <Box sx={progressWrapperSx}>
          <CircularProgress
            size={14}
            thickness={5}
          />
          <Typography
            variant="bodySmall"
            color="text.secondary"
            sx={{ whiteSpace: 'nowrap' }}
          >
            {t('features.toolkits.indexNameWrapper.inProgress', 'In Progress')}
          </Typography>
        </Box>
      )}
    </Box>
  );
}
