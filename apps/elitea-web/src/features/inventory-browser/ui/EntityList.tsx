/**
 * The entities a filter matched, and which one is open.
 *
 * THE SELECTED ROW IS THE ONE THE DETAIL PANE SHOWS, not merely the one that
 * was clicked. The two are different after a filter changes: the open entity
 * may not be in the new list, and a list that highlighted a click instead of
 * the open entity would show nothing selected while the pane still described
 * something. So the caller owns the selection and passes it in.
 */
import Box from '@mui/material/Box';
import List from '@mui/material/List';
import ListItemButton from '@mui/material/ListItemButton';
import ListItemText from '@mui/material/ListItemText';
import Typography from '@mui/material/Typography';

import type { InventoryEntity } from '@/entities/inventory';
import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';

const listSx = { maxHeight: '100%', overflowY: 'auto' } as const;

export interface EntityListProps {
  readonly entities: readonly InventoryEntity[];
  readonly selectedId: string | null;
  readonly isPending: boolean;
  readonly error: string | null;
  readonly onSelect: (entity: InventoryEntity) => void;
}

export function EntityList({
  entities,
  selectedId,
  isPending,
  error,
  onSelect,
}: EntityListProps): React.JSX.Element {
  if (isPending) {
    return (
      <Typography variant="bodyMedium" data-testid="inventory-entities-pending">
        {t('inventory.graph.loading', 'Reading the graph…')}
      </Typography>
    );
  }

  if (error !== null) {
    return (
      <Box data-testid="inventory-entities-error">
        <BannerMessage variant="error" message={error} />
      </Box>
    );
  }

  if (entities.length === 0) {
    // "No entity matches this filter" and "this graph is empty" are the same
    // screen and different facts. The description names the filter as the thing
    // to change, because a user on an empty graph has already been told to
    // ingest one by the Sources tab.
    return (
      <Box data-testid="inventory-entities-empty">
        <NoResultsMessage
          title={t('inventory.graph.emptyTitle', 'No entities')}
          description={t(
            'inventory.graph.empty',
            'Nothing in this graph matches the current filter. Clear it, or ingest a source first.',
          )}
        />
      </Box>
    );
  }

  return (
    <List dense sx={listSx} data-testid="inventory-entity-list">
      {entities.map((entity) => (
        <ListItemButton
          key={entity.id}
          selected={entity.id === selectedId}
          data-testid="inventory-entity-row"
          data-entity-id={entity.id}
          onClick={() => {
            onSelect(entity);
          }}
        >
          <ListItemText
            primary={entity.name}
            secondary={secondaryOf(entity)}
            slotProps={{ secondary: { variant: 'bodySmall' } }}
          />
        </ListItemButton>
      ))}
    </List>
  );
}

/**
 * The one line under an entity's name: its type, its layer, its source and its
 * file, with the empty ones dropped.
 *
 * Joining the raw fields would render `class · · code ·` for a node an older
 * ingestion left half-described — a row that looks damaged rather than one
 * whose graph did not record a layer.
 */
function secondaryOf(entity: InventoryEntity): string {
  return [entity.type, entity.layer, entity.sourceToolkit, entity.filePath]
    .filter((part) => part !== '')
    .join(' · ');
}
