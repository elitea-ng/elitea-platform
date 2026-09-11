/**
 * features/credentials/api/configurations.ts — hand-written endpoint layer
 * for the `/configurations/*` domain (unit A7; manifest API-145..API-160).
 *
 * WHY HAND-WRITTEN, NOT GENERATED: this resource has no OpenAPI schema
 * (confirmed against `services/elitea-main/api/openapi/v2.yaml` and
 * `src/shared/api/generated/**`, neither of which has a `configurations`
 * tag — `entities/credential/model/types.ts`'s own doc comment records the
 * same finding: "No OpenAPI schema exists for this resource (chat/
 * agent-authoring domain, not in the W2 manifest)"). Per R-A5 ("every
 * network call must go through a generated or hand-registered endpoint …
 * and appear in endpoints.manifest.json") these 19 endpoints are
 * hand-registered: every fetcher below calls `eliteaFetch` (the same
 * transport `shared/api/generated/**`'s orval hooks call — this file adds
 * NO new `fetch`/`XMLHttpRequest` site, so R-A1/R-A4 are untouched), and
 * `endpoints.manifest.json` carries one `source:"handwritten"` entry per
 * endpoint (this unit appended them — see that file's own append
 * convention comment). The bottom-of-file stored-connection-check family
 * (3 endpoints) has no baseline/`API-*` precedent — see its own comment.
 *
 * Ported from `apps/elitea-ui/src/api/configurations.js` (RTK Query). This
 * file keeps the URL/parameter/body shape byte-for-byte identical to the
 * baseline (API-145..160's acceptance text); the RTK-specific plumbing
 * (`providesTags`/`invalidatesTags`/`merge`/`serializeQueryArgs`/
 * `onQueryStarted` optimistic patch) has no equivalent here — that
 * responsibility moves to TanStack Query's own cache-key/invalidation
 * model at the hook layer (`./useConfigurations.ts`), which is the correct
 * one-for-one substitute per spec §2.3.
 *
 * Response shapes are NOT documented by any spec, so every field below is
 * read directly off `apps/elitea-ui/src/api/configurations.js` and its
 * consumers (`credential.helpers.js`, `useLoadCredentials.js`,
 * `useCredentialsData.hooks.js`) rather than guessed. `ConfigurationWire`
 * intentionally allows unknown extra keys (`[key: string]: unknown`) —
 * the wire objects are DB-row pass-throughs (`internal/api/v2/...` handler
 * behaviour for this domain is out of this unit's ownership fence to
 * verify against Go source), matching this codebase's `x-elitea-passthrough`
 * convention for genuinely dynamic payloads.
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';

/**
 * `eliteaFetch<T>` always resolves to `{data: T, status, headers}` — the
 * ENVELOPE, never the bare body (see `mutator.ts`'s own doc comment).
 * Every fetcher below goes through this wrapper instead of calling
 * `eliteaFetch` directly, to unwrap `.data` at exactly one place.
 */
export async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

/* ── wire types (snake_case, exactly as the old RTK Query layer read them) ── */

/** A JSON-Schema-shaped node describing one credential type's configurable fields. Recursive and open-ended by nature (server-authored), so only the fields this domain's UI actually reads are typed. */
export interface ConfigSchemaNode {
  readonly type?: string;
  readonly title?: string;
  readonly description?: string;
  readonly default?: unknown;
  readonly enum?: readonly unknown[];
  readonly format?: string;
  readonly secret?: boolean;
  readonly required?: readonly string[];
  readonly properties?: Readonly<Record<string, ConfigSchemaNode>>;
  readonly metadata?: {
    readonly label?: string;
    readonly hidden?: boolean;
    readonly categories?: readonly string[];
    readonly sections?: Readonly<Record<string, ConfigSchemaSection>>;
  };
  readonly [key: string]: unknown;
}

/** One selectable group inside a schema section. */
export interface ConfigSchemaSubsection { readonly name: string; readonly fields?: readonly string[]; }
/** A schema section groups alternative field sets. */
export interface ConfigSchemaSection { readonly required?: boolean; readonly subsections?: readonly ConfigSchemaSubsection[]; }

/** One row of `getAvailableConfigurationsType`'s response — a credential TYPE descriptor, not an instance. */
export interface ConfigurationTypeDescriptor {
  readonly type: string;
  readonly section?: string;
  readonly config_schema: ConfigSchemaNode;
  readonly has_test_connection?: boolean;
  readonly check_connection_label?: string;
  readonly [key: string]: unknown;
}

/** One row of `getConfigurationsList`/`getConfigurationDetail` — a credential INSTANCE. Maps 1:1 to `entities/credential`'s `Credential` after `normalizeCredential` (see `../lib/normalizeCredential.ts`). */
export interface ConfigurationWire {
  readonly id?: string | number;
  readonly uid?: string;
  readonly uuid?: string;
  readonly type: string;
  readonly data?: Readonly<Record<string, unknown>>;
  readonly elitea_title?: string;
  readonly label?: string;
  readonly shared?: boolean;
  readonly section?: string;
  readonly project_id?: string | number;
  readonly is_pinned?: boolean;
  readonly [key: string]: unknown;
}

export interface ConfigurationPageWire {
  readonly items: readonly ConfigurationWire[];
  readonly total: number;
  readonly limit: number;
  readonly offset: number;
  readonly shared?: {
    readonly items: readonly ConfigurationWire[];
    readonly total: number;
    readonly limit?: number;
    readonly offset?: number;
  };
}

/* ── shared query-string helper (byte-for-byte port of `appendParam`) ────── */

/** `exactOptionalPropertyTypes` forbids `{ signal: undefined }` against `RequestInit`'s optional `signal` field. Exported: `./configurationConnections.ts` reuses it rather than duplicating. */
export function withSignal(signal: AbortSignal | undefined): { signal: AbortSignal } | Record<string, never> {
  return signal ? { signal } : {};
}

/** Narrows to the primitives a query-string value can be — avoids `no-base-to-string` on a bare `String()`/template coercion of a genuinely `unknown` value. */
function toQueryParamValue(value: string | number | boolean): string {
  return typeof value === 'string' ? value : String(value);
}

/** `Array.isArray` alone narrows to `any[]`, unsound for `no-unsafe-argument` against `readonly string[]` — this checks every element. */
function isStringArray(value: unknown): value is readonly string[] {
  return Array.isArray(value) && value.every((item) => typeof item === 'string');
}

function appendParam(query: URLSearchParams, key: string, value: string | number | boolean | readonly string[] | undefined): void {
  if (value === undefined || value === '' || value === false) return;
  if (isStringArray(value)) {
    for (const item of value) query.append(key, item);
    return;
  }
  query.append(key, toQueryParamValue(value));
}

/* ── API-145: GET /configurations/available/ ─────────────────────────────── */

/** Real usage note: the baseline accepts arbitrary extra query params via object spread, but no call site (grepped) ever passes one — every caller supplies `section` alone, so this is typed to just that field rather than an open index signature (the mixed shape is also what produced the `unsafe-argument`/`no-base-to-string` findings this rewrite fixes). */
export interface GetAvailableConfigurationsTypeParams {
  readonly section?: string | readonly string[];
}

export function buildAvailableConfigurationsTypeUrl(params: GetAvailableConfigurationsTypeParams = {}): string {
  const query = new URLSearchParams();
  appendParam(query, 'section', params.section);
  return `/configurations/available/?${query.toString()}`;
}

export async function getAvailableConfigurationsType(
  params: GetAvailableConfigurationsTypeParams = {},
  signal?: AbortSignal,
): Promise<ConfigurationTypeDescriptor[]> {
  return fetchData<ConfigurationTypeDescriptor[]>(buildAvailableConfigurationsTypeUrl(params), withSignal(signal));
}

/* ── API-146: GET /configurations/configurations/{projectId} (list) ─────── */

export interface GetConfigurationsListParams {
  readonly projectId: string | number;
  readonly page?: number;
  readonly pageSize?: number;
  readonly type?: string | readonly string[];
  readonly section?: string | readonly string[];
  readonly includeShared?: boolean;
  readonly sharedOffset?: number;
  readonly sharedLimit?: number;
  readonly params?: {
    readonly sort_by?: string;
    readonly sort_order?: string;
    readonly query?: string;
  };
}

const DEFAULT_PAGE_SIZE = 20;

export function buildConfigurationsListUrl(params: GetConfigurationsListParams): string {
  const {
    projectId,
    page = 0,
    pageSize = DEFAULT_PAGE_SIZE,
    type,
    section,
    includeShared = false,
    sharedOffset = 0,
    sharedLimit = pageSize,
    params: extra,
  } = params;
  const { sort_by, sort_order, query: q } = extra ?? { sort_by: 'created_at', sort_order: 'desc', query: '' };

  const search = new URLSearchParams();
  search.append('include_shared', String(includeShared));
  search.append('shared_offset', String(sharedOffset));
  search.append('shared_limit', String(sharedLimit));
  search.append('limit', String(pageSize));
  search.append('offset', String(page * pageSize));
  if (sort_by !== undefined) search.append('sort_by', sort_by);
  if (sort_order !== undefined) search.append('sort_order', sort_order);
  if (q !== undefined) search.append('query', q);
  appendParam(search, 'type', type);
  appendParam(search, 'section', section);

  return `/configurations/configurations/${projectId}?${search.toString()}`;
}

export async function getConfigurationsList(
  params: GetConfigurationsListParams,
  signal?: AbortSignal,
): Promise<ConfigurationPageWire> {
  return fetchData<ConfigurationPageWire>(buildConfigurationsListUrl(params), withSignal(signal));
}

/* ── API-147: GET /configurations/configurations/{projectId} (by type) ──── */

export interface GetConfigurationsByTypeParams {
  readonly projectId: string | number;
  readonly type: string;
  readonly page?: number;
  readonly pageSize?: number;
  readonly params?: Readonly<Record<string, string | number | boolean>>;
}

export function buildConfigurationsByTypeUrl(params: GetConfigurationsByTypeParams): string {
  const { projectId, type, page = 0, pageSize = DEFAULT_PAGE_SIZE, params: extra = {} } = params;
  const search = new URLSearchParams();
  search.append('type', type);
  search.append('limit', String(pageSize));
  search.append('offset', String(page * pageSize));
  for (const [key, value] of Object.entries(extra)) search.append(key, String(value));
  return `/configurations/configurations/${projectId}?${search.toString()}`;
}

export async function getConfigurationsByType(
  params: GetConfigurationsByTypeParams,
  signal?: AbortSignal,
): Promise<ConfigurationPageWire> {
  return fetchData<ConfigurationPageWire>(buildConfigurationsByTypeUrl(params), withSignal(signal));
}

/* ── API-148: GET /configurations/configurations/{projectId} (by section) ── */

export interface GetConfigurationsBySectionParams {
  readonly projectId: string | number;
  readonly section: string | readonly string[];
  readonly page?: number;
  readonly pageSize?: number;
  readonly params?: Readonly<Record<string, string | number | boolean>>;
}

export function buildConfigurationsBySectionUrl(params: GetConfigurationsBySectionParams): string {
  const { projectId, section, page = 0, pageSize = DEFAULT_PAGE_SIZE, params: extra = {} } = params;
  const search = new URLSearchParams();
  search.append('limit', String(pageSize));
  search.append('offset', String(page * pageSize));
  for (const [key, value] of Object.entries(extra)) search.append(key, String(value));
  appendParam(search, 'section', section);
  return `/configurations/configurations/${projectId}?${search.toString()}`;
}

export async function getConfigurationsBySection(
  params: GetConfigurationsBySectionParams,
  signal?: AbortSignal,
): Promise<ConfigurationPageWire> {
  return fetchData<ConfigurationPageWire>(buildConfigurationsBySectionUrl(params), withSignal(signal));
}

/* ── API-149: POST /configurations/configurations/{projectId} ───────────── */

export interface CreateConfigurationBody {
  readonly elitea_title: string;
  readonly label?: string;
  readonly type: string;
  readonly data: Readonly<Record<string, unknown>>;
  readonly shared?: boolean;
}

export async function createConfiguration(
  projectId: string | number,
  body: CreateConfigurationBody,
): Promise<ConfigurationWire> {
  return fetchData<ConfigurationWire>(`/configurations/configurations/${projectId}`, {
    method: 'POST',
    body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json' },
  });
}

/* ── API-150: GET /configurations/configuration/{projectId}/{configId} ──── */

export async function getConfigurationDetail(
  projectId: string | number,
  configId: string | number,
  signal?: AbortSignal,
): Promise<ConfigurationWire> {
  return fetchData<ConfigurationWire>(`/configurations/configuration/${projectId}/${configId}`, withSignal(signal));
}

/* ── API-151: GET /configurations/configurations/{projectId} (shared only) ── */

export interface GetSharedConfigurationsParams {
  readonly projectId: string | number;
  readonly page?: number;
  readonly pageSize?: number;
  readonly params?: Readonly<Record<string, string | number | boolean>>;
}

const EMPTY_SHARED_PAGE = { total: 0, items: [], offset: 0, limit: DEFAULT_PAGE_SIZE } as const;

export async function getSharedConfigurations(
  params: GetSharedConfigurationsParams,
  signal?: AbortSignal,
): Promise<{ readonly total: number; readonly items: readonly ConfigurationWire[]; readonly offset: number; readonly limit: number }> {
  const { projectId, page = 0, pageSize = DEFAULT_PAGE_SIZE, params: extra = {} } = params;
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(extra)) search.append(key, String(value));
  search.append('include_shared', 'true');
  search.append('shared_offset', String(page * pageSize));
  search.append('shared_limit', String(pageSize));
  const response = await fetchData<ConfigurationPageWire>(
    `/configurations/configurations/${projectId}?${search.toString()}`,
    withSignal(signal),
  );
  const { shared } = response;
  if (!shared) return EMPTY_SHARED_PAGE;
  return { total: shared.total, items: shared.items, offset: shared.offset ?? 0, limit: shared.limit ?? DEFAULT_PAGE_SIZE };
}

/* ── API-152: PUT /configurations/configuration/{projectId}/{configId} ──── */

export interface UpdateConfigurationBody {
  readonly elitea_title: string;
  readonly label?: string;
  readonly data: Readonly<Record<string, unknown>>;
  readonly meta?: unknown;
  readonly shared?: boolean;
}

export async function updateConfiguration(
  projectId: string | number,
  configId: string | number,
  body: UpdateConfigurationBody,
): Promise<ConfigurationWire> {
  return fetchData<ConfigurationWire>(`/configurations/configuration/${projectId}/${configId}`, {
    method: 'PUT',
    body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json' },
  });
}

/* ── API-153: DELETE /configurations/configuration/{projectId}/{configId} ── */

export async function deleteConfiguration(
  projectId: string | number,
  configId: string | number,
): Promise<unknown> {
  return fetchData<unknown>(`/configurations/configuration/${projectId}/${configId}`, { method: 'DELETE' });
}

/* ── stored-connection-check family: no baseline/P1 precedent (native Go backend, built in parallel); see file header ── */

/** Legacy-shaped `{success, message?}` result — same family as `configurationConnections.ts`'s `BatchTestResultRow`, declared locally (not imported) to avoid inverting that file's one-way split off this one. `[key: string]: unknown` matches this file's `ConfigurationWire`/`ConfigSchemaNode` passthrough convention. */
export interface StoredConnectionCheckResult { readonly success?: boolean; readonly message?: string; readonly [key: string]: unknown; }

/** One row of the batch response — adds the `id`/`unsupported` fields `BatchTestResultRow` also carries. */
export interface StoredConnectionCheckRow extends StoredConnectionCheckResult { readonly id: string; readonly unsupported?: boolean; }

/** POST /configurations/check_stored_connection/{projectId}/{configId} — re-tests an already-stored config; no body (unlike `testConfigurationConnection`, which must send the not-yet-persisted candidate payload). */
export async function checkStoredConfigurationConnection(projectId: string | number, configId: string | number): Promise<StoredConnectionCheckResult> {
  return fetchData<StoredConnectionCheckResult>(`/configurations/check_stored_connection/${projectId}/${configId}`, { method: 'POST' });
}

/** POST /configurations/check_stored_connections/{projectId} — batch form of the above. */
export async function batchCheckStoredConfigurationConnections(projectId: string | number, configurationIds: readonly string[]): Promise<StoredConnectionCheckRow[]> {
  return fetchData<StoredConnectionCheckRow[]>(`/configurations/check_stored_connections/${projectId}`, {
    method: 'POST',
    body: JSON.stringify({ configuration_ids: configurationIds }),
    headers: { 'Content-Type': 'application/json' },
  });
}

/** POST /configurations/revalidate/{projectId}/{configId} — returns the configuration row, same shape as `updateConfiguration`; no body. */
export async function revalidateConfiguration(projectId: string | number, configId: string | number): Promise<ConfigurationWire> {
  return fetchData<ConfigurationWire>(`/configurations/revalidate/${projectId}/${configId}`, { method: 'POST' });
}
