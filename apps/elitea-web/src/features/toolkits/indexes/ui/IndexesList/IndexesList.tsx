import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';

import { isMissingVectorStoreError, readIndexesListErrorMessage, readIndexesListErrorStatus } from '../../lib/helpers/indexesListError';
import type { IndexRow } from '../../model/indexesStore';
import { IndexListItem } from './IndexListItem';

/**
 * Port of `apps/elitea-ui/src/[fsd]/features/toolkits/indexes/ui/
 * IndexesList/index.jsx` (unit A4a) — the left-rail list of indexes with an
 * "add index" button and a loading skeleton state.
 *
 * THE THIRD STATE (added here). The port had two: loading, and
 * `!indexesList.length` → "Still no indexes created". A FAILED read lands in
 * the second one — the query leaves `data` undefined and the container falls
 * back to an empty array — so the rail asserted "no indexes" about a toolkit
 * whose index list it had not been able to read. See
 * `../../lib/helpers/indexesListError.ts` for the measured case (validation
 * matrix F4: a 400 naming a missing PGVector configuration, rendered as an
 * empty list) and for why the server's own sentence is shown verbatim.
 */
export interface IndexesListProps {
  readonly handleAddIndex: () => void;
  readonly indexesList: readonly IndexRow[];
  readonly onIndexClick: (index: IndexRow) => void;
  readonly currentIndex?: IndexRow | null;
  readonly loading?: boolean;
  /** The list query's rejection, when it failed. `undefined` means the read succeeded — an empty list then really is empty. */
  readonly error?: unknown;
}

const SKELETON_ROW_COUNT = 4;

/**
 * The failed-read body. Split from `IndexesList` for the §3.5 cyclomatic
 * budget (12) — the three conditional lines below all belong to one state and
 * counting them against the list's own loading/empty/rows branching says
 * nothing useful about either.
 */
function IndexesListError({ error }: { readonly error: unknown }): ReactNode {
  const message = readIndexesListErrorMessage(error);
  const status = readIndexesListErrorStatus(error);

  return (
    <Box
      data-testid="indexes-list-error"
      sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}
    >
      <Typography
        variant="bodyMedium"
        color="error.main"
      >
        {status === undefined
          ? t('features.toolkits.indexesList.loadFailed', 'The index list could not be loaded.')
          : t('features.toolkits.indexesList.loadFailedWithStatus', 'The index list could not be loaded ({{status}}).', { status })}
      </Typography>
      {message !== undefined && (
        <Typography
          variant="bodySmall"
          color="text.secondary"
        >
          {message}
        </Typography>
      )}
      {isMissingVectorStoreError(message) && (
        <Typography
          variant="bodySmall"
          color="text.secondary"
        >
          {t(
            'features.toolkits.indexesList.missingVectorStore',
            'Indexes need a vector store. Create a PgVector configuration in Settings → AI Providers, then select it in this toolkit’s Configuration tab.',
          )}
        </Typography>
      )}
    </Box>
  );
}

export function IndexesList(props: IndexesListProps): ReactNode {
  const { handleAddIndex, indexesList, onIndexClick, currentIndex, loading = false, error } = props;

  // A failed read outranks an empty list; on screen the two are otherwise
  // indistinguishable, which is the whole defect.
  const hasError = error !== undefined && error !== null && !loading;

  return (
    <Box
      sx={{
        width: '16.25rem',
        minWidth: '16.25rem',
        padding: '1rem 1.5rem 1rem 0rem',
        borderRight: (theme) => `.0625rem solid ${theme.vars.palette.divider}`,
        height: '100%',
        display: 'flex',
        flexDirection: 'column',
      }}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '1rem' }}>
        <Typography variant="subtitle">INDEXES</Typography>
        <IconButton
          aria-label={t('features.toolkits.indexesList.addIndex', 'Add index')}
          onClick={handleAddIndex}
        >
          <PlusIcon />
        </IconButton>
      </Box>
      {hasError ? (
        <IndexesListError error={error} />
      ) : !indexesList.length && !loading ? (
        <Typography
          variant="bodyMedium"
          color="text.disabled"
        >
          {t('features.toolkits.indexesList.empty', 'Still no indexes created')}
        </Typography>
      ) : (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem', overflowY: 'auto', flexGrow: 1, height: '100%', maxHeight: '100%' }}>
          {loading
            ? Array.from({ length: SKELETON_ROW_COUNT }).map((_, skeletonIndex) => (
                <IndexListItem
                  useMock
                  key={`skeleton-${skeletonIndex}`}
                  index={{ id: `skeleton-${skeletonIndex}`, metadata: {} }}
                />
              ))
            : indexesList.map((index) => (
                <IndexListItem
                  key={index.id}
                  index={index}
                  onIndexClick={onIndexClick}
                  currentIndex={currentIndex}
                />
              ))}
        </Box>
      )}
    </Box>
  );
}
