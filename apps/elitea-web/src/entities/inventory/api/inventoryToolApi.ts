/**
 * The Inventory facade's three routes, and the invoke → poll loop every
 * Inventory screen is built on.
 *
 * THERE IS NO GENERATED CLIENT FOR THIS FACADE. `api/openapi/v2.yaml` declares
 * the DeepWiki trio and not these — the Inventory routes are hand-mounted on
 * the router root (internal/api/v2/inventory/inventory.go:80-84). Adding them
 * to the spec is a six-gate change that belongs to whoever owns the document,
 * so this module calls `eliteaFetch` directly and carries the same three
 * disciplines the generated client would have:
 *
 *   - the FUNCTION shape, never a query hook, for the POST and the DELETE.
 *     orval's `override.query.useQuery` turns every operation into a query, so
 *     an invoke made that way fires on mount, on refocus and on reconnect — an
 *     ingestion started three times, with the screen looking fine.
 *   - `unwrapBody`, because `eliteaFetch` resolves the transport ENVELOPE. A
 *     reader that takes `invocation_id` off the envelope gets `undefined` on a
 *     200 and then polls `undefined` for ever (issue #132's shape).
 *   - the `/api/v2` prefix is NOT written here. `shared/api/http.ts` resolves
 *     the base; the paths below are what the facade's constants say minus that
 *     prefix, exactly as the generated DeepWiki client spells its own.
 *
 * EVERY READ IS AN INVOCATION. Inventory has no REST read surface at all:
 * "what types does this graph hold" is `POST …/invoke` followed by polling
 * until a terminal status. That is why `runInventoryTool` exists — a react-query
 * `queryFn` cannot be a hook, and a screen with six panels would otherwise
 * carry six copies of the loop.
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';
import { invocationIdFrom, isTerminalPoll, terminalOutcome, type InvocationPoll } from '@/entities/provider-run';

import { toolErrorText, toolResultDocument, toolResultText } from '../lib/toolResult';
import type { InventorySettings } from '../model/types';

/**
 * The two toolkit families the descriptor advertises.
 *
 * They are two names for one graph: `inventory` is the toolkit that owns and
 * ingests it, `inventory_search` is the read-only face other agents get. The
 * screens use `inventory` for everything it serves and `inventory_search` for
 * `investigate`, which only that family declares.
 */
export const INVENTORY_FAMILY = 'inventory';
/** The read-only family, and the only one that serves `investigate`. */
export const INVENTORY_SEARCH_FAMILY = 'inventory_search';

/** Which toolkit, in which project, with which settings, a call is about. */
export interface InventoryTarget {
  readonly projectId: string;
  readonly toolkitId: string;
  readonly settings: InventorySettings;
}

/** A tool's own arguments — whatever its `args_schema` declares. */
export type ToolParams = Readonly<Record<string, unknown>>;

function invokePath(target: InventoryTarget, family: string, tool: string): string {
  return `/inventory/tools/${target.projectId}/${family}/${tool}/invoke`;
}

function invocationPath(
  target: InventoryTarget,
  family: string,
  tool: string,
  invocationId: string,
): string {
  return `/inventory/invocations/${target.projectId}/${family}/${tool}/${invocationId}`;
}

/**
 * The invocation envelope.
 *
 * `configuration.parameters` is the TOOLKIT's settings and `parameters` is the
 * tool's own; the provider merges them with the legacy rule — a tool argument
 * overrides a configured one only when it is truthy
 * (`MergeParameters`, internal/apps/inventory/run/params.go:75-95). Sending
 * the settings is not optional decoration: `bucket` and `llm_model` reach the
 * provider through no other field, so an invoke without them reads the default
 * bucket and refuses to ingest.
 */
function buildInventoryRequest(target: InventoryTarget, params: ToolParams) {
  return {
    configuration: { parameters: { ...target.settings } },
    parameters: { ...params },
  };
}

/** Start one invocation and answer its id. */
export async function startInventoryTool(
  target: InventoryTarget,
  family: string,
  tool: string,
  params: ToolParams,
): Promise<string> {
  const response = await eliteaFetch<unknown>(invokePath(target, family, tool), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(buildInventoryRequest(target, params)),
  });
  return invocationIdFrom(
    unwrapBody(response),
    'Inventory accepted the request but returned no invocation to follow.',
  );
}

/** Poll one invocation. */
export async function pollInventoryTool(
  target: InventoryTarget,
  family: string,
  tool: string,
  invocationId: string,
): Promise<InvocationPoll | undefined> {
  const response = await eliteaFetch<unknown>(
    invocationPath(target, family, tool, invocationId),
    { method: 'GET' },
  );
  return unwrapBody(response) as InvocationPoll | undefined;
}

/** Stop one invocation. Answers nothing: the next poll reports `Stopped`. */
export async function cancelInventoryTool(
  target: InventoryTarget,
  family: string,
  tool: string,
  invocationId: string,
): Promise<void> {
  await eliteaFetch<unknown>(invocationPath(target, family, tool, invocationId), {
    method: 'DELETE',
  });
}

/** How often a read's invocation is polled while it runs. */
const READ_POLL_INTERVAL_MS = 700;
/**
 * How long a read may take before it is reported as one that did not finish.
 *
 * Two minutes, which is far longer than any read and far shorter than an
 * ingestion — which is why an ingestion is NOT run through this function. A
 * read that hangs must end as an error a user can see; a promise that never
 * settles is a spinner that never stops and a query that can never be retried.
 */
const READ_TIMEOUT_MS = 120_000;

/** How a finished read is delivered: the document, the text, and the raw poll. */
export interface ToolRun {
  /** The JSON document, for a call made with `output_format: 'json'`. */
  readonly document: Record<string, unknown> | undefined;
  /** The tool's own result text — markdown when no JSON format was asked for. */
  readonly text: string;
  readonly poll: InvocationPoll;
}

function sleep(ms: number, signal: AbortSignal | undefined): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted === true) {
      reject(new Error('The Inventory request was cancelled.'));
      return;
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      reject(new Error('The Inventory request was cancelled.'));
    };
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

export interface RunToolOptions {
  /** react-query passes its own; a cancelled query stops the loop at once. */
  readonly signal?: AbortSignal | undefined;
  readonly intervalMs?: number | undefined;
  readonly timeoutMs?: number | undefined;
}

/**
 * Invoke a read tool and wait for its answer.
 *
 * ONE REQUEST AT A TIME, like `useInvocationPoll`: a poll slower than the
 * interval must not overlap the next one, because `custom_events` are
 * read-once and two overlapping polls would split them. Sequential awaiting
 * gives that for free, which is the reason this is a loop and not an interval.
 *
 * A REFUSAL IS AN ERROR, NOT AN EMPTY RESULT. The provider answers a missing
 * entity with `status: Error` and `error_category: resource_not_found`
 * (fixture.go's `fixtureNotFound`); returning an empty document for it would
 * render "this graph holds nothing" for a mistyped id. `terminalOutcome`
 * classifies it and this throws the provider's own sentence, which is what the
 * screen shows.
 */
export async function runInventoryTool(
  target: InventoryTarget,
  family: string,
  tool: string,
  params: ToolParams,
  options: RunToolOptions = {},
): Promise<ToolRun> {
  const interval = options.intervalMs ?? READ_POLL_INTERVAL_MS;
  const deadline = Date.now() + (options.timeoutMs ?? READ_TIMEOUT_MS);
  const invocationId = await startInventoryTool(target, family, tool, params);

  for (;;) {
    const poll = await pollInventoryTool(target, family, tool, invocationId);
    if (isTerminalPoll(poll) && poll !== undefined) {
      const outcome = terminalOutcome(poll, `Inventory could not run '${tool}'.`);
      // PEELED, because a refusal carries the same result envelope a success
      // does; an unpeeled throw puts the JSON array in the screen's banner.
      if (outcome?.kind === 'failed') throw new Error(toolErrorText(outcome.message));
      return {
        document: toolResultDocument(poll.result),
        text: toolResultText(poll.result),
        poll,
      };
    }
    if (Date.now() >= deadline) {
      // The invocation is left running on purpose: cancelling here would take
      // a long ingestion down because a screen stopped waiting for it, and the
      // provider's own status tools can still report it afterwards.
      throw new Error(`Inventory did not finish '${tool}' in time.`);
    }
    await sleep(interval, options.signal);
  }
}
