/**
 * REST client for the admin AUDIT TRAIL surface — unit A14, issue #200.
 *
 * Four reads, zero writes. That is the whole surface: the audit trail is a
 * record of what happened, and nothing in the product edits it.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as `./adminUsersApi.ts`.
 *
 * The wire contract mirrors the Go handlers in
 * `services/elitea-main/internal/api/v2/eliteacore/audit.go`, which in turn
 * mirror the legacy originals (`legacy/plugins/elitea_core/api/v2/audit*.py`)
 * the existing admin_ui client already speaks to — same paths, same query
 * parameters, same body keys.
 *
 * ## Nothing is reused from `./adminUsersApi.ts`
 *
 * A different endpoint family (`/elitea_core/audit*` vs `/admin/auth_users`), a
 * different row shape, a different query-key namespace and no mutations at all.
 * The only shared code is what both import: `eliteaFetch` and
 * `@/shared/api/unwrap` (R-A6, issue #132 — never a hand-rolled `.data.data`).
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody, unwrapListPage } from '@/shared/api/unwrap';

/** Only `administration` has a handler, server-side as in the legacy API. */
const ADMIN_MODE = 'administration';

/**
 * The event types the audit write path emits. `user` and `system` name the two
 * tabs, which differ ONLY in which of these they ask the server for.
 */
export const USER_EVENT_TYPES = ['api', 'socketio', 'rpc', 'agent', 'tool', 'llm'] as const;
export const SYSTEM_EVENT_TYPES = ['schedule', 'admin_task'] as const;

/** `traces` groups spans by `trace_id`; `spans` lists them individually. */
export type AuditViewMode = 'traces' | 'spans';

/**
 * One row of `GET /elitea_core/audit/administration` — one `audit_events` row.
 *
 * Every field here is a REAL column of `centry.audit_events`, checked against
 * the table that owns the write path
 * (`legacy/plugins/tracing/models/audit_event.py`) and against a live database.
 * That check is not ceremony: the admin Users reference page read a `status`
 * column that has never existed, so its status chip could only ever render one
 * value. Nothing below is a constant dressed as data.
 */
export interface AuditSpanRow {
  readonly id: number;
  readonly timestamp: string | null;
  readonly user_id: number | null;
  readonly user_email: string | null;
  readonly project_id: number | null;
  readonly event_type: string;
  readonly action: string;
  readonly http_method: string | null;
  readonly http_route: string | null;
  readonly status_code: number | null;
  readonly duration_ms: number | null;
  readonly is_error: boolean;
  readonly entity_name: string | null;
  readonly tool_name: string | null;
  readonly model_name: string | null;
  readonly trace_id: string | null;
  readonly span_id: string | null;
  readonly parent_span_id: string | null;
}

/** One row of `GET /elitea_core/audit_traces/administration` — one trace. */
export interface AuditTraceRow {
  readonly trace_id: string;
  readonly start_time: string | null;
  /** Wall-clock duration of the WHOLE trace, not of any single span. */
  readonly duration_ms: number | null;
  readonly span_count: number;
  readonly error_count: number;
  readonly has_error: boolean;
  readonly user_email: string | null;
  readonly project_id: number | null;
  readonly event_types: readonly string[];
  readonly root_action: string | null;
  readonly root_event_type: string | null;
  readonly root_http_method: string | null;
  readonly root_status_code: number | null;
}

/** A heatmap cell. `y === null` means "no events", which is not the same as 0. */
interface AuditHeatmapPoint {
  /** Bucket start, in epoch SECONDS. Formatted into local time by the client. */
  readonly x: number;
  readonly y: number | null;
}

/** One duration band's row of cells. `id` is the band label, e.g. `"1-10s"`. */
interface AuditHeatmapSeries {
  readonly id: string;
  readonly data: readonly AuditHeatmapPoint[];
}

interface AuditHeatmapMetadata {
  readonly interval_seconds: number;
  readonly interval_label: string;
  readonly bucket_count: number;
  readonly range_seconds: number;
  /** The span heatmap reports `total_events`, the trace heatmap `total_traces`. */
  readonly total: number;
}

export interface AuditHeatmap {
  readonly series: readonly AuditHeatmapSeries[];
  readonly metadata: AuditHeatmapMetadata | null;
}

/**
 * The filter set, shared by all four endpoints because the server accepts the
 * same one on all four — and because the chart and the table below it must be
 * drawn over the SAME query. A heatmap filtered differently from its table is a
 * lie about the same data.
 */
export interface AuditQueryFilters {
  readonly search?: string | undefined;
  /** Comma-joined server-side; the page always sends the active tab's whole set. */
  readonly eventTypes?: string | undefined;
  readonly onlyErrors?: boolean | undefined;
  readonly userId?: string | undefined;
  readonly projectId?: string | undefined;
  readonly traceId?: string | undefined;
  readonly dateFrom?: string | undefined;
  readonly dateTo?: string | undefined;
  readonly durationMin?: number | undefined;
  readonly durationMax?: number | undefined;
  /**
   * Cache-buster for the Refresh button. Part of the query KEY and deliberately
   * never put on the wire — react-query would otherwise serve the cached answer
   * for an unchanged filter set, and a refresh control that re-renders the rows
   * it already had is a control that does nothing.
   */
  readonly refreshToken?: number | undefined;
}

export interface AuditListParams extends AuditQueryFilters {
  readonly limit: number;
  readonly offset: number;
  readonly sortBy: string;
  readonly sortOrder: 'asc' | 'desc';
}

/**
 * One query-key namespace for this page's data, declared once.
 *
 * The params object is part of the key, so changing any filter is a new cache
 * entry rather than a silent re-render of the previous answer. Building a key
 * ad hoc at a call site is the read/write namespace split that made saved data
 * look absent in #132 — there are no writes here, but the same rule keeps the
 * four queries from colliding with each other.
 */
const adminAuditKeys = {
  all: ['admin', 'audit'] as const,
  spans: (params: AuditListParams) => ['admin', 'audit', 'spans', params] as const,
  traces: (params: AuditListParams) => ['admin', 'audit', 'traces', params] as const,
  heatmap: (mode: AuditViewMode, filters: AuditQueryFilters) =>
    ['admin', 'audit', 'heatmap', mode, filters] as const,
};

function appendFilters(query: URLSearchParams, filters: AuditQueryFilters): void {
  if (filters.search) query.set('search', filters.search);
  if (filters.eventTypes) query.set('event_type', filters.eventTypes);
  // Only ever sent as `true`: the server reads it as "errors only", so sending
  // `false` would have to mean "successes only", which it does not.
  if (filters.onlyErrors) query.set('is_error', 'true');
  if (filters.userId) query.set('user_id', filters.userId);
  if (filters.projectId) query.set('project_id', filters.projectId);
  if (filters.traceId) query.set('trace_id', filters.traceId);
  if (filters.dateFrom) query.set('date_from', filters.dateFrom);
  if (filters.dateTo) query.set('date_to', filters.dateTo);
  if (filters.durationMin !== undefined) query.set('duration_min', String(filters.durationMin));
  if (filters.durationMax !== undefined) query.set('duration_max', String(filters.durationMax));
}

function buildListUrl(resource: string, params: AuditListParams): string {
  const query = new URLSearchParams({
    limit: String(params.limit),
    offset: String(params.offset),
    sort_by: params.sortBy,
    sort_order: params.sortOrder,
  });
  appendFilters(query, params);
  return `/elitea_core/${resource}/${ADMIN_MODE}?${query.toString()}`;
}

export interface AuditPage<TRow> {
  readonly rows: TRow[];
  readonly total: number;
}

/** `GET /elitea_core/audit/administration` — one page of individual spans. */
export function useAuditSpans(
  params: AuditListParams,
  options: { enabled?: boolean } = {},
): UseQueryResult<AuditPage<AuditSpanRow>, Error> {
  return useQuery({
    queryKey: adminAuditKeys.spans(params),
    enabled: options.enabled ?? true,
    queryFn: async (): Promise<AuditPage<AuditSpanRow>> =>
      unwrapListPage<AuditSpanRow>(await eliteaFetch<unknown>(buildListUrl('audit', params)), 'adminAuditSpans'),
  });
}

/** `GET /elitea_core/audit_traces/administration` — one page of traces. */
export function useAuditTraces(
  params: AuditListParams,
  options: { enabled?: boolean } = {},
): UseQueryResult<AuditPage<AuditTraceRow>, Error> {
  return useQuery({
    queryKey: adminAuditKeys.traces(params),
    enabled: options.enabled ?? true,
    queryFn: async (): Promise<AuditPage<AuditTraceRow>> =>
      unwrapListPage<AuditTraceRow>(await eliteaFetch<unknown>(buildListUrl('audit_traces', params)), 'adminAuditTraces'),
  });
}

/**
 * The heatmap body carries a `data` array AND a sibling `metadata` the rows do
 * not describe, so the transport peel comes from `unwrapBody` (R-A6's
 * sanctioned module for exactly this case) rather than a hand-rolled
 * `resp.data` descent. Only the per-field validation is local.
 *
 * A missing or malformed `metadata` degrades to `null`, which renders as a
 * chart with no caption — never as a wrong bucket width. That matters more than
 * it looks: `interval_seconds` is what turns a clicked cell back into a time
 * RANGE, so a guessed value would drill down into the wrong window.
 */
function readHeatmap(response: unknown, totalKey: 'total_events' | 'total_traces'): AuditHeatmap {
  const body = unwrapBody(response);
  if (typeof body !== 'object' || body === null) return { series: [], metadata: null };
  const { data, metadata } = body as { data?: unknown; metadata?: unknown };
  return {
    series: Array.isArray(data) ? (data as AuditHeatmapSeries[]) : [],
    metadata: readHeatmapMetadata(metadata, totalKey),
  };
}

function readHeatmapMetadata(
  metadata: unknown,
  totalKey: 'total_events' | 'total_traces',
): AuditHeatmapMetadata | null {
  if (typeof metadata !== 'object' || metadata === null) return null;
  const record = metadata as Record<string, unknown>;
  const intervalSeconds = record['interval_seconds'];
  // Without a usable bucket width there is no honest drill-down, so the whole
  // caption is dropped rather than defaulted to a plausible-looking number.
  if (typeof intervalSeconds !== 'number' || intervalSeconds <= 0) return null;
  return {
    interval_seconds: intervalSeconds,
    interval_label: typeof record['interval_label'] === 'string' ? record['interval_label'] : '',
    bucket_count: typeof record['bucket_count'] === 'number' ? record['bucket_count'] : 0,
    range_seconds: typeof record['range_seconds'] === 'number' ? record['range_seconds'] : 0,
    total: typeof record[totalKey] === 'number' ? record[totalKey] : 0,
  };
}

/**
 * `GET /elitea_core/audit_{trace_,}heatmap/administration`.
 *
 * `mode` selects WHICH endpoint, because the two count different things: the
 * span heatmap counts events, the trace heatmap counts traces. One trace of
 * five 30ms spans is five cells in one and a single cell in the other, in a
 * different duration band.
 *
 * Both bounds are required by the server (400 otherwise), so the query stays
 * disabled until the page has them.
 */
export function useAuditHeatmap(
  mode: AuditViewMode,
  filters: AuditQueryFilters,
  options: { enabled?: boolean } = {},
): UseQueryResult<AuditHeatmap, Error> {
  const resource = mode === 'traces' ? 'audit_trace_heatmap' : 'audit_heatmap';
  const totalKey = mode === 'traces' ? 'total_traces' : 'total_events';
  return useQuery({
    queryKey: adminAuditKeys.heatmap(mode, filters),
    enabled: (options.enabled ?? true) && Boolean(filters.dateFrom) && Boolean(filters.dateTo),
    queryFn: async (): Promise<AuditHeatmap> => {
      const query = new URLSearchParams();
      appendFilters(query, filters);
      const response = await eliteaFetch<unknown>(`/elitea_core/${resource}/${ADMIN_MODE}?${query.toString()}`);
      return readHeatmap(response, totalKey);
    },
  });
}
