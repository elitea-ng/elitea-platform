/**
 * REST client for the admin LLM Proxy section — `/api/v2/admin/gateway`.
 *
 * ## What this surface is
 *
 * The section that replaced LiteLLM. Elitea's LLM path is now
 * `services/elitea-llm-gateway`, a standalone service built on maximhq/bifrost's
 * core, and this client serves the parts of Bifrost's own admin UI that this
 * platform can back with real data:
 *
 *   - **Status** (`GET /admin/gateway/status`) — a read-only proxy of the
 *     gateway's `GET /governance/status`. It reports what the gateway HOLDS,
 *     which is not what the governance table CONTAINS: a row that was rejected,
 *     a row that is inert, and a snapshot that is stale because refreshes are
 *     failing are each invisible from the authoring side.
 *   - **Model catalogue** (`GET|PUT|DELETE /admin/gateway/models`) — Bifrost's
 *     Model Catalog and Pricing Overrides. `gateway.gateway_models` is the cost
 *     basis for every billed request. A model with no row is not refused and,
 *     for a token model, is not free: the gateway falls back to a prefix table
 *     and then to a flat invented rate, so the call is billed and budgeted at a
 *     figure nobody chose. Audio is the exception — a per-second rate is never
 *     fabricated, so those calls really are billed zero.
 *
 * ## Prices are per 1M tokens
 *
 * Every price field here is per 1M tokens (per 1M seconds or characters for the
 * audio dimensions). The gateway's cost calculator divides by the same 1M.
 * Sending a per-1k number is a 1000x costing error that nothing downstream
 * would catch, so this client never converts and the form labels the unit.
 *
 * ## An override is permanent until it is cleared
 *
 * Saving a price sets `price_overridden`, and the scheduler's price-sync UPSERT
 * skips those rows (shared migration 0097). That is what makes the edit stick —
 * and also why clearing it matters: a row left overridden never tracks upstream
 * again. `useClearAdminLlmModelOverride` hands it back to the sync, and does NOT
 * delete the row, so the model stays priced until the sync refreshes it.
 *
 * ## The unpriced report is the finding, not a footnote
 *
 * `unpriced` lists (provider, model) pairs that were CALLED in the window and
 * have no catalogue row: a token call billed at an invented rate, or an audio
 * call billed at nothing. Either way the money on the bill is not the money in
 * the counters, and it is the one thing on this screen that is always
 * actionable.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as `./adminGovernanceApi`.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

const GATEWAY_URL = '/admin/gateway';
const STATUS_URL = `${GATEWAY_URL}/status`;
const MODELS_URL = `${GATEWAY_URL}/models`;

/**
 * The `managed_surface` value the server declares on the section this editor
 * owns. Exported so the page's registry keys on the SERVER's word rather than
 * on a section id this app chose to recognise — see `../Configuration.tsx`.
 */
export const LLM_PROXY_MANAGED_SURFACE = 'llm_proxy';

/** The reporting windows the server accepts. An unknown value falls back to 24h. */
export const USAGE_WINDOWS = ['24h', '7d', '30d'] as const;

export type UsageWindow = (typeof USAGE_WINDOWS)[number];

/**
 * One row the gateway refused to load, or loaded and cannot ever apply.
 *
 * These are the two states the authoring page cannot show. `rejected` means the
 * definition did not parse; `inert` means it parsed and matches nothing. Both
 * look identical from the governance table, where the row is simply present and
 * enabled.
 */
export interface GatewayDiagnosticRow {
  readonly id: string;
  readonly type: string;
  readonly name: string;
  readonly reason: string;
}

/**
 * The gateway's own status body.
 *
 * Every field is optional because this type describes a payload the GATEWAY
 * owns and this app only reads. A required field here would be a second
 * specification of that contract, free to drift — and a build that asserted a
 * field the gateway stopped sending would render undefined as though it were a
 * value.
 */
export interface GatewayStatusBody {
  readonly enabled?: boolean;
  readonly rate_limits_enforceable?: boolean;
  /**
   * The public project whose shared credentials and models this gateway reads
   * in addition to the caller's own. An empty string means the second scope is
   * OFF; `undefined` means the gateway is too old to say, which is a different
   * fact and must not be read as "off".
   *
   * The Providers tab compares it against the project elitea-main publishes
   * into. The two come from different services, so a mismatch produces platform
   * credentials that resolve for nobody while every other signal stays healthy.
   */
  readonly shared_project_id?: string;
  readonly store?: {
    readonly has_database?: boolean;
    readonly last_attempt?: string;
    readonly last_success?: string;
    readonly error?: string;
    readonly refresh_interval?: string;
  };
  readonly definitions?: {
    readonly loaded_at?: string;
    readonly rows?: number;
    readonly budgets?: number;
    readonly rate_limits?: number;
    readonly model_configs?: number;
    readonly mcp_allowlists?: number;
    readonly credential_policies?: number;
    readonly routing_rules?: number;
    readonly egress_allowlists?: number;
    readonly rejected?: readonly GatewayDiagnosticRow[];
    readonly inert?: readonly GatewayDiagnosticRow[];
  };
  readonly rate_limiter?: {
    readonly refused?: number;
    readonly degraded?: number;
  };
  /**
   * The MERGED egress allowlist the gateway is enforcing, tagged by source.
   *
   * `env` is the GATEWAY_EGRESS_ALLOWLIST floor the chart sets, `db` is what the
   * Governance page authored, and `effective` is the union actually applied.
   * They are reported apart because "the host I added is not working" has two
   * different answers depending on whether the entry reached the gateway at all.
   *
   * `private_network` is the one an operator most often needs: a private
   * destination is reachable only when an entry names that address or its
   * block. A host name alone does not unlock it, because the gateway never
   * resolves a name to decide.
   */
  readonly egress?: {
    readonly configured?: boolean;
    readonly env?: readonly string[];
    readonly db?: readonly string[];
    readonly effective?: readonly string[];
    readonly dropped?: readonly string[];
    readonly private_network?: boolean;
  };
}

/** What `GET /admin/gateway/status` returns. */
export interface GatewayStatus {
  /**
   * Whether the gateway answered. It leads the screen because every other
   * number describes a snapshot that may be minutes old or absent entirely, and
   * reading them without knowing whether the hop answered is how a stale report
   * gets read as a live one.
   */
  readonly reachable: boolean;
  readonly gateway?: GatewayStatusBody;
  /** The server's own sentence when the gateway could not be read. */
  readonly error?: string;
}

/**
 * The per-1M price fields the gateway's cost path actually reads.
 *
 * Six, not the table's nine. `cache_creation_input_token_cost`,
 * `cache_read_input_token_cost` and `input_cost_per_1m_tokens_above_128k` are
 * written by the price sync and read by nothing, so they are neither shown nor
 * writable here — and the server's upsert never names them, so a value the sync
 * put there survives an override untouched.
 *
 * Not exported: it is a base for the two types below and has no caller of its
 * own, which the knip dead-code gate refuses.
 */
interface LlmModelPrices {
  readonly input_cost_per_1m_tokens: number | null;
  readonly output_cost_per_1m_tokens: number | null;
  readonly input_cost_per_1m_seconds: number | null;
  readonly output_cost_per_1m_seconds: number | null;
  readonly input_cost_per_1m_characters: number | null;
  readonly output_cost_per_1m_characters: number | null;
}

/** One catalogue entry with the usage observed in the requested window. */
export interface LlmModelRow extends LlmModelPrices {
  readonly id: string;
  readonly provider: string;
  readonly model_name: string;
  readonly source?: string;
  readonly source_synced_at?: string;
  readonly last_sync_at?: string;
  readonly updated_at?: string;
  readonly price_overridden: boolean;
  readonly price_overridden_at?: string;
  readonly price_overridden_by?: string;
  readonly requests: number;
  readonly total_tokens: number;
  readonly cost_usd: number;
}

/** A pair that was called in the window and has no catalogue row. */
export interface UnpricedLlmModel {
  readonly provider: string;
  readonly model_name: string;
  readonly requests: number;
  readonly total_tokens: number;
  readonly cost_usd: number;
}

/** What the catalogue read returns. */
export interface LlmModelCatalogue {
  readonly items: readonly LlmModelRow[];
  readonly unpriced: readonly UnpricedLlmModel[];
  readonly window: string;
  /**
   * Whether the catalogue page was capped. The price sync ingests LiteLLM's
   * whole price sheet, so the full table runs to roughly 1800 rows; the server
   * returns the first 200 by (provider, model) and says so here, and the screen
   * turns that into a prompt to search rather than a silently short list.
   */
  readonly truncated?: boolean;
  /**
   * Why the unpriced report could not be produced, when it could not be.
   *
   * Distinct from `error`, which refuses the whole read. An empty `unpriced`
   * renders no alert at all, so a failed check would otherwise look exactly like
   * a deployment where every called model is priced — the single conclusion this
   * panel exists to stop an operator reaching by accident.
   */
  readonly unpriced_error?: string;
  /**
   * Set when the catalogue could not be read. The server answers 200 with an
   * empty list and this reason rather than a 5xx, so the rest of the section
   * stays usable — the same shape the governance list uses.
   */
  readonly error?: string;
}

/**
 * One query-key namespace per surface, declared once.
 *
 * Every mutation invalidates `llmProxyKeys.models`. A key built ad hoc at a call
 * site would be a cache the writes never refresh — the read/write key-namespace
 * split that made saved data look absent in #132. The status key is separate on
 * purpose: a price edit does not change the gateway's snapshot, and
 * invalidating it would refetch the hop on every keystroke-driven save.
 */
const llmProxyKeys = {
  all: ['admin', 'llmProxy'] as const,
  status: () => ['admin', 'llmProxy', 'status'] as const,
  models: ['admin', 'llmProxy', 'models'] as const,
  modelList: (usageWindow: UsageWindow, search: string) =>
    ['admin', 'llmProxy', 'models', usageWindow, search] as const,
};

/**
 * `GET /admin/gateway/status`.
 *
 * Refetched on an interval because it is a live report rather than stored data:
 * the gateway re-reads its definitions on its own poll
 * (`LLM_GOVERNANCE_REFRESH_SEC`, 30 s by default), so a screen left open would
 * otherwise keep showing the snapshot that was current when it loaded — and
 * "the definitions are stale" is one of the very things this panel reports.
 */
export function useGatewayStatus(): UseQueryResult<GatewayStatus, Error> {
  return useQuery({
    queryKey: llmProxyKeys.status(),
    queryFn: async (): Promise<GatewayStatus> => {
      const body = unwrapBody(await eliteaFetch<unknown>(STATUS_URL)) as GatewayStatus | undefined;
      return body ?? { reachable: false };
    },
    refetchInterval: 30_000,
  });
}

/**
 * Fills in what an absent or partial body leaves out.
 *
 * Both error fields are carried through when present and omitted when not — the
 * distinction matters: `error` refuses the whole read, `unpriced_error` says
 * only that the unpriced check did not run, and an empty `unpriced` with
 * neither set is the genuine "nothing unpriced" answer.
 */
function normaliseCatalogue(
  body: LlmModelCatalogue | undefined,
  usageWindow: UsageWindow,
): LlmModelCatalogue {
  // Early return for the absent body, so the field list below is not six
  // optional chains deep.
  if (body === undefined) {
    return { items: [], unpriced: [], window: usageWindow, truncated: false };
  }
  return {
    items: body.items ?? [],
    unpriced: body.unpriced ?? [],
    window: body.window ?? usageWindow,
    truncated: body.truncated ?? false,
    ...(body.error !== undefined ? { error: body.error } : {}),
    ...(body.unpriced_error !== undefined ? { unpriced_error: body.unpriced_error } : {}),
  };
}

/** `GET /admin/gateway/models?window=&q=`. */
export function useAdminLlmModels(
  usageWindow: UsageWindow,
  search: string,
): UseQueryResult<LlmModelCatalogue, Error> {
  return useQuery({
    queryKey: llmProxyKeys.modelList(usageWindow, search),
    queryFn: async (): Promise<LlmModelCatalogue> => {
      const query = new URLSearchParams({ window: usageWindow });
      if (search !== '') query.set('q', search);
      const body = unwrapBody(await eliteaFetch<unknown>(`${MODELS_URL}?${query.toString()}`)) as
        LlmModelCatalogue | undefined;
      return normaliseCatalogue(body, usageWindow);
    },
  });
}

/**
 * What the price dialog collects.
 *
 * Every price is `number | null`, and null is sent rather than omitted: on this
 * surface null MEANS "this model has no rate for that dimension", and omitting
 * the field would leave whatever was previously stored — so clearing a price
 * that should not exist would be impossible.
 */
export interface LlmModelPriceDraft extends LlmModelPrices {
  readonly provider: string;
  readonly model_name: string;
}

/**
 * `PUT /admin/gateway/models` — author a price override.
 *
 * The body carries ONLY the identity and the prices. The server refuses unknown
 * fields, so sending a whole `LlmModelRow` back (with its id, usage counters and
 * provenance) would be a 400 — deliberately: a client that echoed a read row
 * would be asserting values it does not own.
 */
export function useSaveAdminLlmModelPrice(): UseMutationResult<void, Error, LlmModelPriceDraft> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (draft: LlmModelPriceDraft) => {
      await eliteaFetch<unknown>(MODELS_URL, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(draft),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: llmProxyKeys.models }),
  });
}

/**
 * `DELETE /admin/gateway/models/{id}` — hand the row back to the price sync.
 *
 * Named for what it does. The route is DELETE because that is the verb a row
 * editor's remove action sends, but no row is deleted: the stored prices keep
 * applying until the sync refreshes them. Deleting instead would bill every call
 * to that model at zero until the next tick.
 */
export function useClearAdminLlmModelOverride(): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await eliteaFetch<unknown>(`${MODELS_URL}/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: llmProxyKeys.models }),
  });
}
