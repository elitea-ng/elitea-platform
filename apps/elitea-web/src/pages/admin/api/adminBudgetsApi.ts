/**
 * REST client for Admin → Budgets — `/api/v2/elitea_core/{project,user}_budget*`.
 *
 * ## Why this is handwritten
 *
 * The routes ARE described in `v2.yaml` and orval does generate hooks for them.
 * They are re-declared here for one reason: until now the generated client had
 * ZERO non-generated callers. Nothing in the app ever set a budget, so eight
 * cost-budget fields the reference exposed had no UI and two columns the LLM
 * gateway reads (`budget_period`, `nats_fail_mode`) had no writer anywhere in
 * the platform. This module is that caller.
 *
 * It stays hand-written rather than wrapping the generated hooks because the
 * listing's `counts` map and the `nats_fail_mode` tri-state (see below) both
 * need shaping that a generated signature does not give, and because every
 * other admin page in this directory reads the same way — one module, one key
 * namespace, one `…FailureReason`.
 *
 * ## The three-state fail mode
 *
 * `nats_fail_mode` is the only field on this surface with THREE inputs:
 *
 *   - **absent**  — leave the stored policy alone. This is what an edit that
 *     changes only the limit must send, or it silently resets a policy the
 *     operator chose earlier.
 *   - **`null`**  — clear the override back to the platform baseline
 *     (`LLM_BUDGET_NATS_FAIL_MODE`).
 *   - **a mode**  — set it.
 *
 * `buildBudgetWrite` below is what keeps those apart. A form that always sent
 * the field would collapse the first two, which is the defect this whole page
 * exists to remove rather than to add another instance of.
 *
 * ## Enforcement can be off while every write here succeeds
 *
 * A budget is stored in PostgreSQL and enforced by the gateway's NATS counter.
 * With no `GATEWAY_NATS_URL` the gateway serves `/llm` with NO budget
 * enforcement: every PUT on this page still answers 200 and every number still
 * round-trips, and nothing refuses a call. `useBudgetEnforcementOff` reads the
 * gateway's own status for that, and `BudgetEnforcementWarning` renders it.
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

import { useGatewayStatus } from './adminLlmProxyApi';

const ADMIN_MODE = 'administration';
const LIST_URL = `/elitea_core/project_budgets/${ADMIN_MODE}`;

function projectBudgetUrl(projectId: number): string {
  return `/elitea_core/project_budget/${ADMIN_MODE}/${projectId}/budget`;
}

function memberBudgetListUrl(projectId: number): string {
  return `/elitea_core/user_budgets/${ADMIN_MODE}/${projectId}`;
}

function memberBudgetUrl(projectId: number, userId: number): string {
  return `/elitea_core/user_budget/${ADMIN_MODE}/${projectId}/user_budget/${userId}`;
}

/**
 * The periods the server accepts. It holds ONE value, and that is a statement
 * about enforcement: the gateway derives the billing window from the calendar
 * month unconditionally, so any other period would be one nothing bills
 * against. The server refuses anything else with a 400.
 */
export const BUDGET_PERIODS = ['monthly'] as const;

export type BudgetPeriod = (typeof BUDGET_PERIODS)[number];

/**
 * Shared migration 0067's CHECK constraint on
 * `gateway.project_budget.nats_fail_mode`, and the three modes
 * `failmode.ResolveFailMode` matches a per-project override against.
 */
export const NATS_FAIL_MODES = ['tiered_hybrid', 'fail_open', 'fail_closed'] as const;

type NatsFailMode = (typeof NATS_FAIL_MODES)[number];

/** The sentinel the form uses for "inherit the platform baseline" (stored NULL). */
export const INHERIT_FAIL_MODE = 'inherit';

export type FailModeChoice = NatsFailMode | typeof INHERIT_FAIL_MODE;

/**
 * The money and period fields every budget read carries.
 *
 * `monthly_limit` is the AUTHORED ceiling and `effective_limit` the ENFORCED
 * one; they differ for a project that is `enabled: false`, which keeps its
 * number and is unlimited to the gateway. Rendering one for the other is how an
 * exemption reads as an active ceiling.
 */
interface BudgetState {
  readonly monthly_limit?: number | null;
  readonly effective_limit?: number | null;
  readonly limit_source?: string;
  readonly currency?: string;
  readonly enabled?: boolean;
  readonly warning_pct?: number;
  readonly spend?: number | null;
  readonly remaining?: number | null;
  readonly percent_used?: number | null;
  readonly spend_available?: boolean;
  readonly period?: string;
  readonly period_start?: string;
  readonly period_end?: string;
  readonly resets_at?: string;
}

/** One row of the Admin → Budgets table. */
export interface ProjectBudgetRow extends BudgetState {
  readonly project_id: number;
  readonly name: string;
  /** The owner's identity for a personal project; `project_user_<n>` is not a name. */
  readonly display_name: string;
  readonly owner_name: string;
  readonly owner_email: string;
  readonly is_personal: boolean;
}

export interface ProjectBudgetListing {
  readonly rows: readonly ProjectBudgetRow[];
  readonly total: number;
  /** Tab labels, computed over ALL projects rather than the filtered set. */
  readonly counts: Readonly<Record<string, number>>;
}

/** A single project's budget, with the two project-only policy columns. */
export interface ProjectBudget extends BudgetState {
  readonly project_id: number;
  readonly budget_period: string;
  /** `null` means this project authors none and inherits the platform baseline. */
  readonly nats_fail_mode: NatsFailMode | null;
}

export interface MemberBudgetRow extends BudgetState {
  readonly user_id: number;
  readonly name?: string;
  readonly email?: string;
  readonly enforced?: boolean;
}

export interface MemberBudgetListing {
  readonly rows: readonly MemberBudgetRow[];
  readonly total: number;
}

export type ProjectTypeFilter = 'all' | 'team' | 'personal';

export interface ProjectBudgetsQueryParams {
  readonly limit: number;
  readonly offset: number;
  readonly search?: string | undefined;
  readonly projectType?: ProjectTypeFilter | undefined;
}

/** One query-key namespace, declared once (issue #132). */
const adminBudgetsKeys = {
  all: ['admin', 'budgets'] as const,
  list: (params: ProjectBudgetsQueryParams) => ['admin', 'budgets', 'list', params] as const,
  project: (projectId: number) => ['admin', 'budgets', 'project', projectId] as const,
  members: (projectId: number) => ['admin', 'budgets', 'members', projectId] as const,
};

/** The server's own explanation of a refusal, when it gave one. */
export function adminBudgetFailureReason(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  if (failure.kind !== 'http') return undefined;
  const body = failure.body;
  if (typeof body !== 'object' || body === null) return undefined;
  const record = body as { message?: unknown; error?: unknown };
  const reason = typeof record.message === 'string' ? record.message : record.error;
  return typeof reason === 'string' && reason !== '' ? reason : undefined;
}

function buildListUrl(params: ProjectBudgetsQueryParams): string {
  const query = new URLSearchParams({
    limit: String(params.limit),
    offset: String(params.offset),
  });
  if (params.search !== undefined && params.search !== '') query.set('search', params.search);
  // 'all' is the ABSENCE of the filter, not a value the server knows.
  if (params.projectType !== undefined && params.projectType !== 'all') {
    query.set('project_type', params.projectType);
  }
  return `${LIST_URL}?${query.toString()}`;
}

function readCounts(response: unknown): Readonly<Record<string, number>> {
  const body = unwrapBody(response);
  if (typeof body !== 'object' || body === null) return {};
  const counts = (body as { counts?: unknown }).counts;
  if (typeof counts !== 'object' || counts === null) return {};
  const result: Record<string, number> = {};
  for (const [key, value] of Object.entries(counts)) {
    if (typeof value === 'number') result[key] = value;
  }
  return result;
}

async function fetchProjectBudgetsPage(
  params: ProjectBudgetsQueryParams,
): Promise<ProjectBudgetListing> {
  const response = await eliteaFetch<unknown>(buildListUrl(params));
  const { rows, total } = unwrapListPage<ProjectBudgetRow>(response, 'adminProjectBudgets');
  return { rows, total, counts: readCounts(response) };
}

export function useProjectBudgets(
  params: ProjectBudgetsQueryParams,
): UseQueryResult<ProjectBudgetListing, Error> {
  return useQuery({
    queryKey: adminBudgetsKeys.list(params),
    queryFn: () => fetchProjectBudgetsPage(params),
  });
}

/**
 * One project's full budget, including the two policy columns the LISTING does
 * not carry. The edit dialog reads this when it opens rather than editing the
 * row it was launched from: the row has the money but not the policy, and a
 * dialog that defaulted the policy fields would write a value nobody chose over
 * one that was already stored.
 */
export function useProjectBudget(
  projectId: number | undefined,
): UseQueryResult<ProjectBudget, Error> {
  return useQuery({
    queryKey: adminBudgetsKeys.project(projectId ?? 0),
    enabled: projectId !== undefined,
    queryFn: async (): Promise<ProjectBudget> =>
      unwrapBody(await eliteaFetch<unknown>(projectBudgetUrl(projectId ?? 0))) as ProjectBudget,
  });
}

export function useMemberBudgets(
  projectId: number | undefined,
): UseQueryResult<MemberBudgetListing, Error> {
  return useQuery({
    queryKey: adminBudgetsKeys.members(projectId ?? 0),
    enabled: projectId !== undefined,
    queryFn: async (): Promise<MemberBudgetListing> => {
      const response = await eliteaFetch<unknown>(memberBudgetListUrl(projectId ?? 0));
      const { rows, total } = unwrapListPage<MemberBudgetRow>(response, 'adminMemberBudgets');
      return { rows, total };
    },
  });
}

/** What the dialog collects. `failMode` is absent when the form did not offer it. */
export interface BudgetFormValues {
  readonly monthlyLimit: number | null;
  readonly enabled: boolean;
  readonly softAlertPct: number | null;
  readonly budgetPeriod?: BudgetPeriod | undefined;
  readonly failMode?: FailModeChoice | undefined;
}

/**
 * Turns the form into the wire payload, and is where the tri-state is kept.
 *
 * `budget_period` and `nats_fail_mode` are OMITTED when the form did not offer
 * them (the member dialog), because the server refuses both at member scope.
 * `nats_fail_mode` is sent as an explicit `null` only for the deliberate
 * "inherit" choice — never as a side effect of leaving a field untouched.
 */
function buildBudgetWrite(values: BudgetFormValues): Record<string, unknown> {
  const payload: Record<string, unknown> = {
    monthly_limit: values.monthlyLimit,
    enabled: values.enabled,
  };
  if (values.softAlertPct !== null) payload['soft_alert_pct'] = values.softAlertPct;
  if (values.budgetPeriod !== undefined) payload['budget_period'] = values.budgetPeriod;
  if (values.failMode !== undefined) {
    payload['nats_fail_mode'] = values.failMode === INHERIT_FAIL_MODE ? null : values.failMode;
  }
  return payload;
}

export interface ProjectBudgetWrite {
  readonly projectId: number;
  readonly values: BudgetFormValues;
}

export function useSaveProjectBudget(): UseMutationResult<void, Error, ProjectBudgetWrite> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ projectId, values }: ProjectBudgetWrite) => {
      await eliteaFetch<unknown>(projectBudgetUrl(projectId), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(buildBudgetWrite(values)),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminBudgetsKeys.all }),
  });
}

/**
 * Clears a project back to the platform default.
 *
 * This is NOT `enabled: false`. That stores "deliberately exempt" — an authored
 * decision that keeps the old ceiling on screen and pins the threshold chosen
 * with it. A cleared project has no authored row and follows the platform as it
 * changes.
 */
export function useClearProjectBudget(): UseMutationResult<void, Error, number> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (projectId: number) => {
      await eliteaFetch<unknown>(projectBudgetUrl(projectId), { method: 'DELETE' });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminBudgetsKeys.all }),
  });
}

export interface MemberBudgetWrite {
  readonly projectId: number;
  readonly userId: number;
  readonly values: BudgetFormValues;
}

export function useSaveMemberBudget(): UseMutationResult<void, Error, MemberBudgetWrite> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ projectId, userId, values }: MemberBudgetWrite) => {
      await eliteaFetch<unknown>(memberBudgetUrl(projectId, userId), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(buildBudgetWrite(values)),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminBudgetsKeys.all }),
  });
}

export interface MemberBudgetClear {
  readonly projectId: number;
  readonly userId: number;
}

export function useClearMemberBudget(): UseMutationResult<void, Error, MemberBudgetClear> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ projectId, userId }: MemberBudgetClear) => {
      await eliteaFetch<unknown>(memberBudgetUrl(projectId, userId), { method: 'DELETE' });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminBudgetsKeys.all }),
  });
}

/**
 * Whether the gateway is reachable AND reports that it cannot enforce (gap
 * G12).
 *
 * The signal is `rate_limits_enforceable`, and the field name understates what
 * it measures. The gateway sets it from `limiter.Enabled()`, which is true
 * exactly when it holds a NATS counter — and the NATS counter is what the
 * BUDGET path admits against too. So a gateway started without
 * `GATEWAY_NATS_URL` reports `rate_limits_enforceable: false`, serves `/llm`
 * with no budget enforcement, and every budget on this page still stores,
 * reads back and displays perfectly while refusing nothing.
 *
 * Three states, and only one of them warns:
 *
 *   - the gateway did not answer (`reachable !== true`) → NOTHING. An
 *     unreachable hop is not evidence that enforcement is off, and warning on
 *     it would turn a transient network fault into a claim about policy.
 *   - the field is absent → NOTHING, for the same reason
 *     `SharedScopeWarning` treats `undefined` as "too old to say".
 *   - reachable and explicitly `false` → warn.
 */
export function useBudgetEnforcementOff(): boolean {
  const status = useGatewayStatus();
  if (status.data?.reachable !== true) return false;
  return status.data.gateway?.rate_limits_enforceable === false;
}
