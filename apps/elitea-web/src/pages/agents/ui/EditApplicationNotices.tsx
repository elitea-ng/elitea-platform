import type { ReactNode } from 'react';

import Typography from '@mui/material/Typography';

import type { MarketplaceOrigin } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { agentSaveErrorMessage } from '../lib/agentSaveErrorMessage';
import { MarketplaceOriginBanner } from './MarketplaceOriginBanner';

export interface EditApplicationNoticesProps {
  readonly origin: MarketplaceOrigin | undefined;
  readonly isError: boolean;
  readonly saveError: unknown;
}

/** What sits above the editor tabs: where a hired agent came from, a failed load and a failed save. */
export function EditApplicationNotices({ origin, isError, saveError }: EditApplicationNoticesProps): ReactNode {
  return (
    <>
      {origin !== undefined && <MarketplaceOriginBanner origin={origin} />}
      {isError && (
        <Typography role="alert" variant="bodyMedium">
          {t('pages.agents.editApplication.error', 'Failed to load this agent.')}
        </Typography>
      )}
      {saveError !== undefined && (
        <Typography role="alert" variant="bodyMedium">
          {agentSaveErrorMessage(
            saveError,
            t('pages.agents.editApplication.saveError', 'Failed to save your changes.'),
          )}
        </Typography>
      )}
    </>
  );
}
