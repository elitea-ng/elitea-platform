/**
 * The Sources panel: what this Inventory toolkit ingests from, and the control
 * that ingests it.
 *
 * IT IS A TABLE AND NOT A CARD LIST, which is where it departs from the legacy
 * screen deliberately. The legacy panel was a 320-pixel drawer beside a graph
 * canvas, so a card was all that fitted; this application gives the sources a
 * full tab, and the four numbers a user compares between sources — entities,
 * relations, when it last ran, what went wrong — are what a table is for.
 *
 * ADDING A SOURCE IS NOT DONE HERE. `sources` is a field of the TOOLKIT, saved
 * by a `PUT` of the whole toolkit row, and the toolkit form already renders it
 * from the descriptor's own `toolkit_types` list. A second editor for the same
 * field is a second thing to keep in step with the descriptor, and the one
 * that is not schema-driven is the one that goes stale. The panel says where
 * the field is instead.
 */
import Box from '@mui/material/Box';
import Stack from '@mui/material/Stack';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import type { IngestionStatus, InventorySource } from '@/entities/inventory';
import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';

import { SourceStatusChip } from './SourceStatusChip';

const sectionSx = { display: 'flex', flexDirection: 'column', gap: 2 } as const;
const headerSx = { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 1 } as const;
const numberCellSx = { whiteSpace: 'nowrap' } as const;
const rowActionsSx = { justifyContent: 'flex-end' } as const;

export interface InventorySourcesPanelProps {
  readonly sources: readonly InventorySource[];
  /**
   * What the PROVIDER says is running, which is not the same as what this
   * browser started. An ingestion begun in another tab, by another user of the
   * project, or before this page was opened is invisible to the local run
   * controller — and starting a second one against a busy provider is refused
   * with a message about slots that reads as a failure of the click.
   */
  readonly ingestion: IngestionStatus;
  readonly isPending: boolean;
  /** The status read failed; the configured sources are still listed. */
  readonly statusError: string | null;
  /** The source an ingestion is running for, or null. */
  readonly runningSourceId: string | null;
  /** False when the toolkit configures no `llm_model`: ingestion would be refused. */
  readonly canIngest: boolean;
  readonly onIngest: (sourceToolkitId: string) => void;
  readonly onStop: () => void;
  readonly onAddSource: () => void;
  /** Takes the source out of `sources`. Its ENTITIES stay in the graph. */
  readonly onRemoveSource: (sourceToolkitId: string) => void;
}

export function InventorySourcesPanel({
  sources,
  ingestion,
  isPending,
  statusError,
  runningSourceId,
  canIngest,
  onIngest,
  onStop,
  onAddSource,
  onRemoveSource,
}: InventorySourcesPanelProps): React.JSX.Element {
  if (isPending) {
    return (
      <Box data-testid="inventory-sources-pending">
        <Typography variant="bodyMedium">
          {t('inventory.sources.loading', 'Reading the source status…')}
        </Typography>
      </Box>
    );
  }

  return (
    <Box sx={sectionSx} data-testid="inventory-sources-panel">
      <Box sx={headerSx}>
        <Typography variant="headingSmall">{t('inventory.sources.title', 'Sources')}</Typography>
        <Stack direction="row" spacing={1}>
          {runningSourceId === null ? null : (
            <BaseBtn variant="alarm" size="small" data-testid="inventory-stop-ingestion" onClick={onStop}>
              {t('inventory.sources.stop', 'Stop ingestion')}
            </BaseBtn>
          )}
          <BaseBtn variant="secondary" size="small" data-testid="inventory-add-source" onClick={onAddSource}>
            {t('inventory.sources.add', 'Add source')}
          </BaseBtn>
        </Stack>
      </Box>

      {ingestion.running ? (
        <BannerMessage
          variant="info"
          message={
            ingestion.source === ''
              ? t('inventory.sources.providerBusy', 'An ingestion is already running for this inventory.')
              : t('inventory.sources.providerBusySource', 'An ingestion is already running for {{source}}.', {
                  source: ingestion.source,
                })
          }
        />
      ) : null}

      {statusError === null ? null : (
        // NOT an empty list. The configured sources below are real; only their
        // status could not be read, and saying "no sources" here would send a
        // user to add ones they already have.
        <BannerMessage
          variant="error"
          message={t('inventory.sources.statusFailed', 'The ingestion status could not be read, so the states below are unknown.')}
        />
      )}

      {canIngest ? null : (
        <BannerMessage
          variant="warning"
          message={t('inventory.sources.noModel', 'This toolkit configures no LLM model, so ingestion is refused. Set llm_model in the toolkit settings.')}
        />
      )}

      {sources.length === 0 ? (
        <Box data-testid="inventory-no-sources">
          <NoResultsMessage
            title={t('inventory.sources.emptyTitle', 'No sources configured')}
            description={t(
              'inventory.sources.empty',
              'Add a source toolkit to start building the knowledge graph.',
            )}
          />
        </Box>
      ) : (
        <Table size="small" data-testid="inventory-sources-table">
          <TableHead>
            <TableRow>
              <TableCell>{t('inventory.sources.column.source', 'Source')}</TableCell>
              <TableCell>{t('inventory.sources.column.status', 'Status')}</TableCell>
              <TableCell align="right">{t('inventory.sources.column.entities', 'Entities')}</TableCell>
              <TableCell align="right">{t('inventory.sources.column.relations', 'Relations')}</TableCell>
              <TableCell>{t('inventory.sources.column.branch', 'Branch')}</TableCell>
              <TableCell align="right">{t('inventory.sources.column.actions', 'Actions')}</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {sources.map((source) => (
              <SourceRow
                key={source.toolkitId === '' ? `orphan:${source.name}` : source.toolkitId}
                source={source}
                busy={runningSourceId !== null || ingestion.running}
                running={runningSourceId === source.toolkitId && source.toolkitId !== ''}
                canIngest={canIngest}
                onIngest={onIngest}
                onRemove={onRemoveSource}
              />
            ))}
          </TableBody>
        </Table>
      )}
    </Box>
  );
}

interface SourceRowProps {
  readonly source: InventorySource;
  /** Some ingestion is running; a second one would be refused by the provider. */
  readonly busy: boolean;
  readonly running: boolean;
  readonly canIngest: boolean;
  readonly onIngest: (sourceToolkitId: string) => void;
  readonly onRemove: (sourceToolkitId: string) => void;
}

/** `—` for a count nobody reported, which is not the same fact as zero. */
function countText(value: number | null): string {
  return value === null ? '—' : String(value);
}

function SourceRow({ source, busy, running, canIngest, onIngest, onRemove }: SourceRowProps): React.JSX.Element {
  // A source with no toolkit id is one whose entities are in the graph and
  // which the toolkit no longer names. It cannot be re-ingested — there is no
  // id to send — so the row reports it and offers nothing.
  const orphan = source.toolkitId === '';
  return (
    <TableRow data-testid="inventory-source-row" data-source-id={source.toolkitId}>
      <TableCell>
        <Stack spacing={0.5}>
          <Typography variant="bodyMedium">{source.name}</Typography>
          <Typography variant="bodySmall" color="text.secondary">
            {orphan
              ? t('inventory.sources.orphan', 'No longer configured — its entities are still in the graph.')
              : source.type}
          </Typography>
          {source.errorMessage === '' ? null : (
            <Typography variant="bodySmall" color="error" data-testid="inventory-source-error">
              {source.errorMessage}
            </Typography>
          )}
        </Stack>
      </TableCell>
      <TableCell>
        <SourceStatusChip status={running ? 'in_progress' : source.status} />
      </TableCell>
      <TableCell align="right" sx={numberCellSx}>
        {countText(source.entityCount)}
      </TableCell>
      <TableCell align="right" sx={numberCellSx}>
        {countText(source.relationCount)}
      </TableCell>
      <TableCell>{source.branch === '' ? '—' : source.branch}</TableCell>
      <TableCell align="right">
        {orphan ? null : (
          <Stack direction="row" spacing={1} sx={rowActionsSx}>
            <BaseBtn
              variant="secondary"
              size="small"
              data-testid="inventory-run-ingestion"
              disabled={busy || !canIngest}
              onClick={() => {
                onIngest(source.toolkitId);
              }}
            >
              {t('inventory.sources.ingest', 'Run ingestion')}
            </BaseBtn>
            <BaseBtn
              variant="text"
              size="small"
              data-testid="inventory-remove-source"
              disabled={busy}
              onClick={() => {
                onRemove(source.toolkitId);
              }}
            >
              {t('inventory.sources.remove', 'Remove')}
            </BaseBtn>
          </Stack>
        )}
      </TableCell>
    </TableRow>
  );
}
