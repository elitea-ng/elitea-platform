/** The entities displayed either side of a drop inside a folder; null at an end. */
export interface ItemNeighbours {
  readonly above: number | null;
  readonly below: number | null;
}

/**
 * The neighbours that put `entityId` at `toIndex` of the displayed order `ids`
 * (the index it has once it lands, as `arrayMove` counts it). The server turns
 * them into a position, so the client never does position arithmetic; "above"
 * is the larger position, since a folder's items sort DESCENDING. Undefined
 * when the entity is not displayed, the index is outside the list, or the drop
 * would leave it where it is.
 */
export function neighboursAt(ids: readonly number[], entityId: number, toIndex: number): ItemNeighbours | undefined {
  const from = ids.indexOf(entityId);
  if (from === -1 || toIndex === from || toIndex < 0 || toIndex >= ids.length) return undefined;
  const rest = ids.filter((id) => id !== entityId);
  return { above: rest[toIndex - 1] ?? null, below: rest[toIndex] ?? null };
}

/**
 * The folder's `item_ids` with `entityId` moved between `above` and `below`,
 * the optimistic picture of a drop before the server answers. A neighbour the
 * list does not hold is ignored; with neither, the entity goes to the top.
 */
export function placeItem(itemIds: readonly number[], entityId: number, { above, below }: ItemNeighbours): number[] {
  const rest = itemIds.filter((id) => id !== entityId);
  const afterAbove = above === null ? -1 : rest.indexOf(above);
  const beforeBelow = below === null ? -1 : rest.indexOf(below);
  let at = 0;
  if (afterAbove !== -1) at = afterAbove + 1;
  else if (beforeBelow !== -1) at = beforeBelow;
  return [...rest.slice(0, at), entityId, ...rest.slice(at)];
}
