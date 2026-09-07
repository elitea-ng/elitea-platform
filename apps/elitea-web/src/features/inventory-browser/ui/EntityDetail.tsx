/**
 * One entity, and the edges that reach it.
 *
 * A NEIGHBOUR IS A LINK, not a label. Clicking one opens it, which is what
 * makes this a graph browser rather than a table with a details pane — the
 * legacy screen's "expand connections" menu did the same thing on a canvas.
 * The caller decides what opening means, because the list and this pane share
 * one selection.
 */
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Link from '@mui/material/Link';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';

import type { InventoryEntity, InventoryNeighbour } from '@/entities/inventory';
import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';

const paneSx = { display: 'flex', flexDirection: 'column', gap: 1.5 } as const;
const chipRowSx = { display: 'flex', flexWrap: 'wrap', gap: 0.5 } as const;
const idSx = { wordBreak: 'break-all' } as const;

export interface EntityDetailProps {
  readonly entity: InventoryEntity | undefined;
  readonly neighbours: readonly InventoryNeighbour[];
  readonly isPending: boolean;
  readonly error: string | null;
  readonly onOpenNeighbour: (entityId: string) => void;
}

export function EntityDetail({
  entity,
  neighbours,
  isPending,
  error,
  onOpenNeighbour,
}: EntityDetailProps): React.JSX.Element {
  if (isPending) {
    return (
      <Typography variant="bodyMedium" data-testid="inventory-entity-pending">
        {t('inventory.entity.loading', 'Reading the entity…')}
      </Typography>
    );
  }

  if (error !== null) {
    // The provider's own sentence. A refusal here is nearly always
    // `resource_not_found` for an id that is in a listing and not in the loaded
    // graph, and only the provider can say which graph it looked in.
    return (
      <Box data-testid="inventory-entity-error">
        <BannerMessage variant="error" message={error} />
      </Box>
    );
  }

  if (entity === undefined) {
    return (
      <Typography variant="bodyMedium" color="text.secondary" data-testid="inventory-entity-none">
        {t('inventory.entity.none', 'Select an entity to see what it is and what it touches.')}
      </Typography>
    );
  }

  return (
    <Box sx={paneSx} data-testid="inventory-entity-detail">
      <Typography variant="headingSmall">{entity.name}</Typography>
      <Box sx={chipRowSx}>
        {entity.type === '' ? null : <Chip size="small" color="primary" label={entity.type} />}
        {entity.layer === '' ? null : (
          <Chip size="small" color="secondary" variant="outlined" label={entity.layer} />
        )}
        {entity.sourceToolkit === '' ? null : <Chip size="small" label={entity.sourceToolkit} />}
      </Box>

      <Field label={t('inventory.entity.id', 'ID')} value={entity.id} testId="inventory-entity-id" />
      {entity.filePath === '' ? null : (
        <Field
          label={t('inventory.entity.file', 'File location')}
          value={entity.filePath}
          testId="inventory-entity-file"
        />
      )}

      <Box>
        <Typography variant="labelSmall" color="text.secondary">
          {t('inventory.entity.relations', 'Relations')}
        </Typography>
        {neighbours.length === 0 ? (
          <Typography variant="bodySmall" color="text.secondary" data-testid="inventory-entity-no-relations">
            {t('inventory.entity.noRelations', 'Nothing in this graph links to this entity.')}
          </Typography>
        ) : (
          <Stack spacing={0.5} data-testid="inventory-entity-relations">
            {neighbours.map((neighbour) => (
              <Typography
                key={`${neighbour.direction}:${neighbour.relationType}:${neighbour.entityId}`}
                variant="bodySmall"
              >
                <Link
                  component="button"
                  type="button"
                  underline="hover"
                  data-testid="inventory-neighbour"
                  data-entity-id={neighbour.entityId}
                  onClick={() => {
                    onOpenNeighbour(neighbour.entityId);
                  }}
                >
                  {neighbour.entityId}
                </Link>{' '}
                {describeEdge(neighbour)}
              </Typography>
            ))}
          </Stack>
        )}
      </Box>
    </Box>
  );
}

interface FieldProps {
  readonly label: string;
  readonly value: string;
  readonly testId: string;
}

function Field({ label, value, testId }: FieldProps): React.JSX.Element {
  return (
    <Box>
      <Typography variant="labelSmall" color="text.secondary">
        {label}
      </Typography>
      <Typography variant="bodySmall" sx={idSx} data-testid={testId}>
        {value}
      </Typography>
    </Box>
  );
}

/**
 * The edge, described.
 *
 * The DIRECTION is what makes a relation readable: "calls" and "is called by"
 * are opposite facts and the provider reports them as one relation type with a
 * direction beside it. Dropping the direction — which the first version of this
 * pane did — renders a caller and a callee identically.
 */
function describeEdge(neighbour: InventoryNeighbour): string {
  const type = neighbour.relationType === '' ? t('inventory.entity.related', 'related') : neighbour.relationType;
  if (neighbour.direction === 'incoming') {
    return t('inventory.entity.incoming', '← {{relation}}', { relation: type });
  }
  if (neighbour.direction === 'outgoing') {
    return t('inventory.entity.outgoing', '→ {{relation}}', { relation: type });
  }
  return type;
}
