/**
 * State and handlers for Admin › LLM Governance (#218).
 *
 * Kept out of the page component for the same reason every other admin page
 * does it: the page file is layout and identity, the behaviour is testable
 * without rendering, and the two do not grow into one file.
 *
 * ## The draft is a typed form, and the payload is built from it
 *
 * A governance row's `data` is an open JSONB document whose shape depends on
 * `type`. The dialog does not edit raw JSON: it renders the fields the chosen
 * type actually has, and `draftToData` assembles the document. That is the
 * difference between a control an operator can use and a text area that
 * produces a row the gateway rejects for a reason nobody sees.
 *
 * A field left empty is OMITTED from the payload rather than sent as zero or
 * empty string. Those are different states on this surface and the server keeps
 * them apart: a budget with no `limit_usd` is unlimited, and a rate limit with
 * no `requests_per_min` does not limit requests. Sending 0 for either would
 * author a ceiling of nothing.
 */
import { useCallback, useMemo, useState } from 'react';

import {
  governanceFailureReason,
  isGlobalGovernanceType,
  useCreateGovernanceRow,
  useDeleteGovernanceRow,
  useGovernanceRows,
  useUpdateGovernanceRow,
  type GovernanceRow,
  type GovernanceType,
} from './api/adminGovernanceApi';

/** One weighted routing target as the dialog edits it: all strings. */
export interface RoutingTargetDraft {
  readonly provider: string;
  readonly model: string;
  readonly weight: string;
}

/**
 * The dialog's working copy of a row.
 *
 * Every value is a STRING, including the numbers. An MUI number input hands
 * back a string, and coercing on each keystroke makes a half-typed "0." jump
 * back to "0"; the coercion happens once, in `draftToData`, where an
 * unparsable value can be left out instead of becoming a silent zero.
 */
export interface GovernanceDraft {
  readonly id?: string | undefined;
  readonly type: GovernanceType;
  readonly name: string;
  readonly enabled: boolean;

  // scope (every type)
  readonly scopeProjectIds: string;
  readonly scopeProviders: string;
  readonly scopeModels: string;

  // budget
  readonly budgetIsUnlimited: boolean;
  readonly budgetLimitUsd: string;
  readonly budgetPeriod: string;
  readonly budgetSoftAlertPct: string;
  readonly budgetNatsFailMode: string;

  // rate_limit
  readonly tokensPerMin: string;
  readonly requestsPerMin: string;

  // credential_policy
  readonly ratePolicy: string;

  // mcp_allowlist
  readonly mcpAllowlist: string;

  // egress_allowlist
  readonly egressAllowlist: string;

  // routing_rule
  readonly cel: string;
  readonly priority: string;
  readonly targets: readonly RoutingTargetDraft[];
}

export const EMPTY_DRAFT: GovernanceDraft = {
  type: 'budget',
  name: '',
  enabled: true,
  scopeProjectIds: '',
  scopeProviders: '',
  scopeModels: '',
  budgetIsUnlimited: false,
  budgetLimitUsd: '',
  budgetPeriod: 'monthly',
  budgetSoftAlertPct: '80',
  budgetNatsFailMode: '',
  tokensPerMin: '',
  requestsPerMin: '',
  ratePolicy: 'billed',
  mcpAllowlist: '',
  egressAllowlist: '',
  cel: '',
  priority: '0',
  targets: [{ provider: '', model: '', weight: '1' }],
};

/** Splits a comma or newline separated list into trimmed, non-empty entries. */
function splitList(raw: string): string[] {
  return raw
    .split(/[\n,]/)
    .map((part) => part.trim())
    .filter((part) => part !== '');
}

/** Parses a list of integers, dropping anything that is not one. */
function splitIntList(raw: string): number[] {
  return splitList(raw)
    .map((part) => Number.parseInt(part, 10))
    .filter((value) => Number.isFinite(value));
}

/** Joins a stored list back into the comma-separated form the field shows. */
function joinList(value: unknown): string {
  return Array.isArray(value) ? value.map((entry) => String(entry)).join(', ') : '';
}

function readRecord(source: unknown, key: string): Record<string, unknown> {
  if (typeof source !== 'object' || source === null) return {};
  const value = (source as Record<string, unknown>)[key];
  return typeof value === 'object' && value !== null ? (value as Record<string, unknown>) : {};
}

function readString(source: Record<string, unknown>, key: string, fallback = ''): string {
  const value = source[key];
  if (typeof value === 'string') return value;
  if (typeof value === 'number') return String(value);
  return fallback;
}

/** The `scope` selector, or undefined when the draft constrains nothing. */
function draftToScope(draft: GovernanceDraft): Record<string, unknown> | undefined {
  // A global type carries no scope, and the server refuses one. Writing scope
  // fields the dialog no longer shows would make a saved row unsavable on the
  // next edit, with a 400 the operator cannot act on because the field that
  // caused it is not on screen.
  if (isGlobalGovernanceType(draft.type)) return undefined;
  const scope: Record<string, unknown> = {};
  const projectIds = splitIntList(draft.scopeProjectIds);
  if (projectIds.length > 0) scope.project_ids = projectIds;
  // `model_config` uses providers and models as its ALLOWLIST rather than as a
  // selector, which is why they are offered for it too.
  const providers = splitList(draft.scopeProviders);
  if (providers.length > 0) scope.providers = providers;
  const models = splitList(draft.scopeModels);
  if (models.length > 0) scope.models = models;
  return Object.keys(scope).length > 0 ? scope : undefined;
}

function draftToBudget(draft: GovernanceDraft): Record<string, unknown> {
  const budget: Record<string, unknown> = {
    is_unlimited: draft.budgetIsUnlimited,
    period: draft.budgetPeriod,
  };
  const limit = Number.parseFloat(draft.budgetLimitUsd);
  // Omitted when unlimited or unparsable: "no ceiling" and "a ceiling of 0" are
  // different rows, and the second one blocks every request.
  if (!draft.budgetIsUnlimited && Number.isFinite(limit)) budget.limit_usd = limit;
  const softPct = Number.parseInt(draft.budgetSoftAlertPct, 10);
  if (Number.isFinite(softPct)) budget.soft_alert_pct = softPct;
  if (draft.budgetNatsFailMode !== '') budget.nats_fail_mode = draft.budgetNatsFailMode;
  return budget;
}

function draftToRateLimit(draft: GovernanceDraft): Record<string, unknown> {
  const rateLimit: Record<string, unknown> = {};
  const tokens = Number.parseInt(draft.tokensPerMin, 10);
  if (Number.isFinite(tokens)) rateLimit.tokens_per_min = tokens;
  const requests = Number.parseInt(draft.requestsPerMin, 10);
  if (Number.isFinite(requests)) rateLimit.requests_per_min = requests;
  return rateLimit;
}

function draftToRoutingRule(draft: GovernanceDraft): Record<string, unknown> {
  const priority = Number.parseInt(draft.priority, 10);
  return {
    cel: draft.cel,
    priority: Number.isFinite(priority) ? priority : 0,
    targets: draft.targets
      .filter((target) => target.provider.trim() !== '' || target.model.trim() !== '')
      .map((target) => ({
        provider: target.provider.trim(),
        model: target.model.trim(),
        weight: Number.parseFloat(target.weight),
      })),
  };
}

/**
 * Assembles the `data` document from a draft.
 *
 * Only the groups the chosen type uses are written. A row carrying leftovers
 * from another type would still load — the gateway reads the group its `type`
 * names — but it would show an operator fields on the next edit that have no
 * effect, which is the same confusion by a quieter route.
 */
export function draftToData(draft: GovernanceDraft): Record<string, unknown> {
  const data: Record<string, unknown> = {};
  const scope = draftToScope(draft);
  if (scope !== undefined) data.scope = scope;

  switch (draft.type) {
    case 'budget':
      data.budget = draftToBudget(draft);
      break;
    case 'rate_limit':
      data.rate_limit = draftToRateLimit(draft);
      break;
    case 'credential_policy':
      data.credential = { rate_policy: draft.ratePolicy };
      break;
    case 'mcp_allowlist':
      data.mcp = { allowlist: splitList(draft.mcpAllowlist) };
      break;
    case 'egress_allowlist':
      data.egress = { allowlist: splitList(draft.egressAllowlist) };
      break;
    case 'routing_rule':
      Object.assign(data, draftToRoutingRule(draft));
      break;
    case 'model_config':
      // The scope IS the definition. Nothing else to write.
      break;
  }
  return data;
}

/** Rebuilds a draft from a stored row, for the edit dialog. */
export function rowToDraft(row: GovernanceRow): GovernanceDraft {
  const scope = readRecord(row.data, 'scope');
  const budget = readRecord(row.data, 'budget');
  const rateLimit = readRecord(row.data, 'rate_limit');
  const credential = readRecord(row.data, 'credential');
  const mcp = readRecord(row.data, 'mcp');
  const egress = readRecord(row.data, 'egress');
  const rawTargets = (row.data as Record<string, unknown>).targets;

  return {
    ...EMPTY_DRAFT,
    id: row.id,
    type: row.type as GovernanceType,
    name: row.name,
    enabled: row.enabled,
    scopeProjectIds: joinList(scope.project_ids),
    scopeProviders: joinList(scope.providers),
    scopeModels: joinList(scope.models),
    budgetIsUnlimited: budget.is_unlimited === true,
    budgetLimitUsd: readString(budget, 'limit_usd'),
    budgetPeriod: readString(budget, 'period', 'monthly'),
    budgetSoftAlertPct: readString(budget, 'soft_alert_pct', '80'),
    budgetNatsFailMode: readString(budget, 'nats_fail_mode'),
    tokensPerMin: readString(rateLimit, 'tokens_per_min'),
    requestsPerMin: readString(rateLimit, 'requests_per_min'),
    ratePolicy: readString(credential, 'rate_policy', 'billed'),
    mcpAllowlist: joinList(mcp.allowlist),
    egressAllowlist: joinList(egress.allowlist),
    cel: readString(row.data, 'cel'),
    priority: readString(row.data, 'priority', '0'),
    targets: Array.isArray(rawTargets) && rawTargets.length > 0
      ? rawTargets.map((entry) => {
          const target = (typeof entry === 'object' && entry !== null ? entry : {}) as Record<string, unknown>;
          return {
            provider: readString(target, 'provider'),
            model: readString(target, 'model'),
            weight: readString(target, 'weight', '0'),
          };
        })
      : EMPTY_DRAFT.targets,
  };
}

/**
 * The inline weight-sum hint. UX ONLY — the server re-verifies the sum on every
 * write and rejects a rule that fails, whatever this showed
 * (design-governance-config-authoring §3.1).
 */
export function targetWeightSum(targets: readonly RoutingTargetDraft[]): number {
  return targets.reduce((total, target) => {
    const weight = Number.parseFloat(target.weight);
    return Number.isFinite(weight) ? total + weight : total;
  }, 0);
}

export interface GatewayGovernancePageState {
  readonly rows: readonly GovernanceRow[];
  readonly isLoading: boolean;
  readonly loadError: string | undefined;
  readonly search: string;
  readonly onSearchChange: (value: string) => void;
  readonly draft: GovernanceDraft | undefined;
  readonly isNew: boolean;
  readonly saveError: string | undefined;
  readonly isSaving: boolean;
  readonly onCreate: () => void;
  readonly onEdit: (row: GovernanceRow) => void;
  readonly onDraftChange: (patch: Partial<GovernanceDraft>) => void;
  readonly onCancel: () => void;
  readonly onSave: () => void;
  readonly onDelete: (row: GovernanceRow) => void;
  readonly deleteError: string | undefined;
}

export function useGatewayGovernancePage(): GatewayGovernancePageState {
  const query = useGovernanceRows();
  const create = useCreateGovernanceRow();
  const update = useUpdateGovernanceRow();
  const remove = useDeleteGovernanceRow();

  const [search, setSearch] = useState('');
  const [draft, setDraft] = useState<GovernanceDraft | undefined>(undefined);

  const rows = useMemo(() => {
    const all = query.data ?? [];
    const needle = search.trim().toLowerCase();
    if (needle === '') return all;
    return all.filter(
      (row) => row.name.toLowerCase().includes(needle) || row.type.toLowerCase().includes(needle),
    );
  }, [query.data, search]);

  const onCreate = useCallback(() => setDraft(EMPTY_DRAFT), []);
  const onEdit = useCallback((row: GovernanceRow) => setDraft(rowToDraft(row)), []);
  const onCancel = useCallback(() => setDraft(undefined), []);

  const onDraftChange = useCallback((patch: Partial<GovernanceDraft>) => {
    setDraft((current) => (current === undefined ? current : { ...current, ...patch }));
  }, []);

  const onSave = useCallback(() => {
    if (draft === undefined) return;
    const payload = {
      id: draft.id,
      type: draft.type,
      name: draft.name.trim(),
      data: draftToData(draft),
      enabled: draft.enabled,
    };
    const mutation = draft.id === undefined ? create : update;
    // The dialog closes only on SUCCESS. A failed save keeps the operator's
    // work on screen with the server's reason beside it; closing would discard
    // a rule they cannot get back.
    mutation.mutate(payload, { onSuccess: () => setDraft(undefined) });
  }, [create, draft, update]);

  const onDelete = useCallback((row: GovernanceRow) => remove.mutate(row.id), [remove]);

  return {
    rows,
    isLoading: query.isPending,
    loadError:
      query.error === null || query.error === undefined
        ? undefined
        : (governanceFailureReason(query.error) ?? query.error.message),
    search,
    onSearchChange: setSearch,
    draft,
    isNew: draft?.id === undefined,
    saveError: governanceFailureReason(create.error ?? update.error),
    isSaving: create.isPending || update.isPending,
    onCreate,
    onEdit,
    onDraftChange,
    onCancel,
    onSave,
    onDelete,
    deleteError: governanceFailureReason(remove.error),
  };
}
