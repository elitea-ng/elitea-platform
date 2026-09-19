/**
 * `features/agents/api/configurations.ts` — hand-registered fetchers for
 * exactly the two `/configurations/*` operations `useCreateConfiguration.ts`
 * needs (`POST /configurations/configurations/{projectId}`,
 * `POST /configurations/check_connection/{projectId}/{configType}`).
 *
 * **Duplicated, not imported, from `features/credentials/api/configurations.ts`
 * / `configurationConnections.ts`** (which already implement the FULL
 * `/configurations/*` surface, API-145..160) — `no-sideways-features`
 * forbids `features/agents` importing `features/credentials`. Same
 * per-slice-duplication convention this session has used repeatedly
 * (`api/useSelectedProjectId.ts`, `__tests__/testUtils.tsx`); the target
 * endpoints already carry real `endpoints.manifest.json` entries
 * (`credentials.createConfiguration`, `credentials.testConfigurationConnection`)
 * — this unit adds `"features/agents"` to both entries' `usedBy` arrays
 * (the manifest's own append convention, `check-endpoint-manifest.mjs`'s
 * header comment, item 3: "keep it current") rather than registering
 * duplicate ids for the same wire endpoint.
 *
 * Body/URL shapes copied byte-for-byte from the credentials file (which
 * itself cites `apps/elitea-ui/src/api/configurations.js` — not re-verified
 * against Go source here, out of this unit's tracing budget; the
 * credentials unit's own doc comment already records "No OpenAPI schema
 * exists for this resource").
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';

async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

export interface ConfigurationWire {
  readonly id?: string | number;
  readonly uid?: string;
  readonly type: string;
  readonly data?: Readonly<Record<string, unknown>>;
  readonly elitea_title?: string;
  readonly label?: string;
  readonly [key: string]: unknown;
}

export interface CreateConfigurationBody {
  readonly elitea_title: string;
  readonly label?: string;
  readonly type: string;
  readonly data: Readonly<Record<string, unknown>>;
}

/** API-149 (`credentials.createConfiguration`). */
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

export interface TestConnectionResult {
  readonly error?: string;
  readonly tools?: readonly unknown[];
  readonly requires_authorization?: boolean;
  readonly auth_metadata?: unknown;
  readonly [key: string]: unknown;
}

/** API-154 (`credentials.testConfigurationConnection`). */
export async function testConfigurationConnection(
  projectId: string | number,
  configType: string,
  body: Readonly<Record<string, unknown>>,
): Promise<TestConnectionResult> {
  return fetchData<TestConnectionResult>(`/configurations/check_connection/${projectId}/${configType}`, {
    method: 'POST',
    body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json' },
  });
}

/**
 * API-146 (`credentials.getConfigurationsList`) — the same list the toolkit
 * form and the credential picker read, narrowed to the one question #937
 * asks: which saved credentials does this project answer to?
 *
 * `include_shared=true` so a credential shared INTO this project resolves
 * too; a toolkit that points at one is not broken, and a warning that says
 * otherwise would be a false claim about the user's data.
 */
export interface ConfigurationPageWire {
  readonly items: readonly ConfigurationWire[];
  readonly total: number;
  readonly shared?: { readonly items: readonly ConfigurationWire[]; readonly total: number };
}

/** The baseline's own picker page size (`useCredentialsData.hooks.js`: 500) — a resolution check reads every row, it does not paginate. */
const CREDENTIAL_PAGE_SIZE = 500;

export function buildConfigurationsListUrl(projectId: string | number, section: string): string {
  const search = new URLSearchParams({
    include_shared: 'true',
    shared_offset: '0',
    shared_limit: String(CREDENTIAL_PAGE_SIZE),
    limit: String(CREDENTIAL_PAGE_SIZE),
    offset: '0',
    section,
  });
  return `/configurations/configurations/${projectId}?${search.toString()}`;
}

export async function getConfigurationsList(projectId: string | number, section: string, signal?: AbortSignal): Promise<ConfigurationPageWire> {
  return fetchData<ConfigurationPageWire>(buildConfigurationsListUrl(projectId, section), signal ? { signal } : {});
}
