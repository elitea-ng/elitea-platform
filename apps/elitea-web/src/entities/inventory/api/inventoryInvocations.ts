/**
 * Start, poll and stop one invocation, published as ONE symbol.
 *
 * `useInventoryTool` covers every READ, and two callers cannot use it: an
 * ingestion and a question both have to watch their invocation while it runs —
 * the progress a user reads comes from `custom_events`, which are READ-ONCE, so
 * a caller that only awaits the answer throws every progress line away. Those
 * two drive the loop themselves through these three functions and
 * `entities/provider-run`'s `useInvocationPoll`.
 *
 * Grouped for the slice budget, the same as `inventoryDocuments` — and because
 * the three are useless apart: a start with no poll is a run nobody watches,
 * and a cancel with no id has nothing to cancel.
 */
import { cancelInventoryTool, pollInventoryTool, startInventoryTool } from './inventoryToolApi';

/** The invoke → poll → cancel trio, for a run that must be watched. */
export const inventoryInvocations = Object.freeze({
  start: startInventoryTool,
  poll: pollInventoryTool,
  cancel: cancelInventoryTool,
});
