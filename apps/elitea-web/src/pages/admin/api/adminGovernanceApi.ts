/**
 * REST client for LLM governance — `/api/v2/admin/gateway/governance` (#218).
 *
 * ## What this surface is for
 *
 * `gateway.governance_config` holds the definitions the LLM gateway enforces on
 * every `/llm` request: budget ceilings, per-minute rate limits, provider and
 * model allowlists, MCP server allowlists, credential billing policy, and CEL
 * routing rules. One row is one definition, with a `scope` saying which requests
 * it applies to.
 *
 * The admin Configuration page does NOT author these, and its LLM Governance
 * section says so. That page is a flat form over one value document; a
 * governance corpus is a list of scoped rows, and the row editor is the only
 * shape that can express it.
 *
 * ## The server decides, always
 *
 * Every write is validated server-side (`internal/api/gateway/governance.go`):
 * the CEL expression is compiled, the routing weights are re-checked against
 * 1.0, and a scope naming teams is refused. This client does not duplicate any
 * of that as a gate — inline hints are UX, and a rule that fails on the server
 * is rejected whatever the editor showed (design-governance-config-authoring
 * §3.1). Its refusals are surfaced verbatim, because they are specific and
 * actionable and a generic "Failed to save" would throw them away.
 *
 * ## Enforcement is not instant, and the page says so
 *
 * The gateway re-reads this table on a poll (`LLM_GOVERNANCE_REFRESH_SEC`,
 * 30 s by default). A definition saved here takes effect within that window,
 * not on the next request. There is no event-driven reload to shorten it, on
 * purpose: a replica that missed an event must still converge.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as `./adminSecretsApi`.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { EliteaApiError, eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody, unwrapListPage } from '@/shared/api/unwrap';

const GOVERNANCE_URL = '/admin/gateway/governance';

function governanceUrl(id: string): string {
  return `${GOVERNANCE_URL}/${encodeURIComponent(id)}`;
}

/**
 * The row `type` values the gateway understands.
 *
 * `budget_alert` is deliberately ABSENT. That row exists in the same table and
 * is written by the separate `PUT /admin/gateway/budget-alerts` surface, which
 * validates the two keys it carries. Offering it here as a free-form JSON row
 * would give an operator a second, unvalidated way to write the platform's
 * soft-alert config, and the two would disagree about what a valid value is.
 */
export const GOVERNANCE_TYPES = [
  'budget',
  'rate_limit',
  'model_config',
  'mcp_allowlist',
  'credential_policy',
  'routing_rule',
  'egress_allowlist',
] as const;

export type GovernanceType = (typeof GOVERNANCE_TYPES)[number];

/**
 * The row types that are GLOBAL: they carry no scope, and the dialog withholds
 * the scope fields for them.
 *
 * `egress_allowlist` is global because half of what it governs — whether the
 * gateway's SSRF-safe dialer is relaxed for the self-hosted provider classes —
 * is decided by bifrost with no project in hand. A scoped entry could only be
 * half-honoured, so the server refuses one on write and the gateway rejects one
 * that reaches it anyway. Showing the fields would invite the operator to
 * author exactly the entry that is refused.
 */
export const GLOBAL_GOVERNANCE_TYPES: readonly GovernanceType[] = ['egress_allowlist'];

/** Whether this type carries a scope at all. */
export function isGlobalGovernanceType(type: GovernanceType): boolean {
  return GLOBAL_GOVERNANCE_TYPES.includes(type);
}

/**
 * The row type this page LISTS but must never edit.
 *
 * `PUT /admin/gateway/budget-alerts` owns it and validates its two keys. It is
 * not in GOVERNANCE_TYPES, so this page's editor has no field for either key —
 * and `draftToData` writes only the groups the chosen type names, so saving one
 * of these rows here would drop `enabled` and `threshold_pct` and leave a row
 * that still looks configured. The list shows it, because an operator looking
 * for their governance rows should see every row that exists; the actions are
 * withheld.
 */
const READ_ONLY_GOVERNANCE_TYPE = 'budget_alert';

/** Whether this row may be edited or deleted from this page. */
export function isEditableGovernanceRow(row: GovernanceRow): boolean {
  return row.type !== READ_ONLY_GOVERNANCE_TYPE;
}

/** The three billing treatments a credential-policy row can carry. */
export const RATE_POLICIES = ['billed', 'zero-rate-metered', 'excluded'] as const;

/** The four budget periods. */
export const BUDGET_PERIODS = ['daily', 'weekly', 'monthly', 'yearly'] as const;

/** The three NATS fail modes a budget row can override. */
export const NATS_FAIL_MODES = ['tiered_hybrid', 'fail_open', 'fail_closed'] as const;

/**
 * One governance row, as the server stores and returns it.
 *
 * `data` is left as an open record on purpose. Its shape depends on `type`, the
 * server owns the validation, and a narrower type here would be a second
 * specification of the same contract — free to drift, and drifting silently.
 */
export interface GovernanceRow {
  readonly id: string;
  readonly type: string;
  readonly section: string;
  readonly name: string;
  readonly data: Readonly<Record<string, unknown>>;
  readonly enabled: boolean;
  readonly created_at?: string;
  readonly updated_at?: string;
}

/** What a create or update sends. `id` is absent on create. */
export interface GovernanceWrite {
  readonly id?: string | undefined;
  readonly type: string;
  readonly name: string;
  readonly data: Record<string, unknown>;
  readonly enabled: boolean;
}

/**
 * One query-key namespace, declared once.
 *
 * Every mutation invalidates `governanceKeys.all`, so a key built ad hoc at a
 * call site would be a cache the writes never refresh — the read/write
 * key-namespace split that made saved data look absent in #132.
 */
const governanceKeys = {
  all: ['admin', 'governance'] as const,
  list: () => ['admin', 'governance', 'list'] as const,
};

/**
 * The server's own explanation of a refusal, when it gave one.
 *
 * The refusals this surface produces are the whole point of the server-side
 * validation, and each one tells the operator what to change:
 *
 *   - `400 {"error":"CEL compile error: …"}` — the expression does not compile.
 *   - `400 {"error":"CEL expression references team_id, which the gateway
 *     cannot evaluate: …"}` — a variable that type-checks and would never have
 *     a value. The rule would look valid and never match.
 *   - `400 {"error":"routing target weights must sum to 1.0"}`.
 *   - `400 {"error":"scope.team_ids is not supported: …"}`.
 *   - `409 {"error":"a governance entry with this section/type/name already
 *     exists"}` — the unique key is `(section, type, name)`.
 *
 * A 403 is among them: `configuration.governance` gates every route here, and
 * an administration-mode role without it is refused with a body rather than
 * with the re-auth flow (issue 93 changed the shared 403 policy).
 */
export function governanceFailureReason(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  if (failure.kind !== 'http') return undefined;
  const body = failure.body;
  if (typeof body !== 'object' || body === null) return undefined;
  const record = body as { error?: unknown; message?: unknown };
  const reason = typeof record.error === 'string' ? record.error : record.message;
  return typeof reason === 'string' && reason !== '' ? reason : undefined;
}

/** `GET /admin/gateway/governance` — every authored definition. */
export function useGovernanceRows(): UseQueryResult<GovernanceRow[], Error> {
  return useQuery({
    queryKey: governanceKeys.list(),
    queryFn: async (): Promise<GovernanceRow[]> =>
      unwrapListPage<GovernanceRow>(await eliteaFetch<unknown>(GOVERNANCE_URL), 'adminGovernance').rows,
  });
}

/** `POST /admin/gateway/governance`. */
export function useCreateGovernanceRow(): UseMutationResult<void, Error, GovernanceWrite> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (row: GovernanceWrite) => {
      await eliteaFetch<unknown>(GOVERNANCE_URL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ type: row.type, name: row.name, data: row.data, enabled: row.enabled }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: governanceKeys.all }),
  });
}

/** `PUT /admin/gateway/governance/{id}`. */
export function useUpdateGovernanceRow(): UseMutationResult<void, Error, GovernanceWrite> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (row: GovernanceWrite) => {
      if (row.id === undefined) throw new Error('cannot update a governance row without an id');
      await eliteaFetch<unknown>(governanceUrl(row.id), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ type: row.type, name: row.name, data: row.data, enabled: row.enabled }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: governanceKeys.all }),
  });
}

/** `DELETE /admin/gateway/governance/{id}`. */
export function useDeleteGovernanceRow(): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await eliteaFetch<unknown>(governanceUrl(id), { method: 'DELETE' });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: governanceKeys.all }),
  });
}

/** The answer to an ad-hoc CEL check. */
export interface CelValidation {
  readonly valid: boolean;
  /** The compiler's message, absent when the expression compiled. */
  readonly error?: string | undefined;
}

/**
 * `POST /admin/gateway/governance/validate-cel` — compile an expression without
 * saving anything.
 *
 * It answers 200 with `{valid:false, error}` for a bad expression rather than a
 * 4xx, so a compile failure is a RESULT here and not a mutation error. The
 * caller renders it beside the field; only a transport failure rejects.
 */
export function useValidateCel(): UseMutationResult<CelValidation, Error, string> {
  return useMutation({
    mutationFn: async (cel: string): Promise<CelValidation> => {
      const body = unwrapBody(
        await eliteaFetch<unknown>(`${GOVERNANCE_URL}/validate-cel`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ cel }),
        }),
      );
      if (typeof body !== 'object' || body === null) return { valid: false };
      const record = body as { valid?: unknown; error?: unknown };
      return {
        valid: record.valid === true,
        error: typeof record.error === 'string' ? record.error : undefined,
      };
    },
  });
}
