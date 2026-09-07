/**
 * One entity, and what it is joined to.
 *
 * TWO READS, and the second is the point of the screen: `get_entity` says what
 * the entity IS, and `get_entity_neighbors` says what it is CONNECTED to. A
 * graph browser that showed only the first would be a table with extra steps.
 *
 * THE PARAMETER NAMES COME FROM THE DESCRIPTOR, which is the contract the
 * provider is tested against — `entity_name` for `get_entity`, `entity_id` for
 * `get_entity_neighbors`. They are genuinely different keys for the same
 * value, and both are sent under both names: a screen that guessed one gets a
 * `resource_not_found` from a graph that holds the entity, which reads as
 * "this entity does not exist" on the row the user just clicked.
 */
import { useMemo } from 'react';

import {
  INVENTORY_FAMILY,
  inventoryDocuments,
  useInventoryTool,
  type InventoryEntity,
  type InventoryNeighbour,
  type InventoryTarget,
} from '@/entities/inventory';

/** How deep `get_entity_neighbors` walks. One hop: the entity's own edges. */
const NEIGHBOUR_DEPTH = 1;

/** What the detail pane renders. */
export interface EntityDetailView {
  readonly entity: InventoryEntity | undefined;
  readonly neighbours: readonly InventoryNeighbour[];
  readonly isPending: boolean;
  /** The entity could not be read — usually because it is not in this graph. */
  readonly error: string | null;
}

/**
 * Every spelling of "which entity", sent together.
 *
 * Cheap, and it removes a whole class of failure: the two tools this feature
 * calls declare different argument names for the same value, the fixture
 * runner reads a third set, and an engine may read a fourth. A body carrying
 * all of them is refused by none of them.
 */
function entityArguments(entityId: string): Record<string, unknown> {
  return { entity_id: entityId, entity_name: entityId, entity: entityId, id: entityId };
}

export function useEntityDetail(
  target: InventoryTarget,
  entityId: string | null,
): EntityDetailView {
  const enabled = entityId !== null && entityId !== '';
  const id = entityId ?? '';
  const detail = useInventoryTool(
    target,
    INVENTORY_FAMILY,
    'get_entity',
    { ...entityArguments(id), include_relations: true },
    { enabled },
  );
  const neighbours = useInventoryTool(
    target,
    INVENTORY_FAMILY,
    'get_entity_neighbors',
    { ...entityArguments(id), depth: NEIGHBOUR_DEPTH },
    { enabled },
  );

  const entity = useMemo(
    () => inventoryDocuments.entity(detail.data?.document),
    [detail.data],
  );
  const related = useMemo(
    () => inventoryDocuments.neighbours(neighbours.data?.document),
    [neighbours.data],
  );

  return {
    entity,
    // The neighbour read failing does NOT hide the entity: knowing what
    // something is, without knowing what it touches, is still an answer.
    neighbours: related,
    isPending: enabled && detail.isPending,
    error: detail.error instanceof Error ? detail.error.message : null,
  };
}
