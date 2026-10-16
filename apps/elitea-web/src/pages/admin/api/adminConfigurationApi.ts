/**
 * REST client for the admin CONFIGURATION surface — unit A14, issue #200.
 *
 * Three calls: the section list, one section's values, and the save. The
 * reference client (`admin_ui`'s `configurationApi.js`) declares fifteen; the
 * other twelve drive Pylon runtime administration — plugin reload, remote plugin
 * YAML, plugin updates, pylon log tailing, maintenance splash — and there is
 * nothing here to call. They are not declared in this module at all, and the
 * sections that would have used them render the server's own reason instead.
 * See `../Configuration.tsx`.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as `./adminSchedulesApi.ts`.
 *
 * The wire contract mirrors the Go handler in
 * `services/elitea-main/internal/api/v2/admin/config_values.go`, which in turn
 * keeps pylon's path, modes and body keys
 * (`legacy/plugins/admin/api/v2/plugin_config_values.py`) so the existing
 * admin_ui client speaks to the same endpoint unchanged.
 *
 * ## What is reused
 *
 * `EliteaApiError`/`eliteaFetch` and the failure-reason reader pattern from
 * `./adminSchedulesApi.ts`. Nothing else: a different endpoint, a different
 * payload shape, its own query-key namespace.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { EliteaApiError, eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

/** Only `administration` may read or write configuration, server-side and in pylon before it. */
const ADMIN_MODE = 'administration';

const SCHEMAS_URL = `/admin/plugin_config_schemas/${ADMIN_MODE}`;

function sectionValuesUrl(sectionId: string): string {
  return `/admin/plugin_config_values/${ADMIN_MODE}/${encodeURIComponent(sectionId)}`;
}

/**
 * One field of a section, as the server declares it.
 *
 * This is a subset of what the schema carries. `sync_targets`/`_is_sync_target`
 * are still omitted — they describe fanning a value out to a second pylon, which
 * has no meaning here — and declaring them would imply this page honours them.
 *
 * The `enum_source` family USED to be omitted for the same reason, on the
 * grounds that its endpoint answered 501 because "the toolkit registry is a
 * Pylon surface". That stopped being true: `plugin_config_suggestions` now reads
 * the digest-pinned SDK toolkit snapshot, and the Guardrails section's fields
 * declare all three of these.
 */
export interface AdminConfigField {
  readonly key: string;
  readonly type: string;
  readonly title: string;
  readonly description?: string;
  readonly format?: string;
  readonly enum?: readonly string[];
  readonly default?: unknown;
  /** The element type of an `array` field, when the schema declares one. */
  readonly items?: { readonly type?: string };
  /**
   * The value shape of an `object` field — the two guardrail tool maps declare
   * `{type: 'array', items: {type: 'string'}}`.
   *
   * Without it an object field has no editable shape and falls through to the
   * unsupported row, which is the correct answer for an object whose values
   * could be anything.
   */
  readonly additionalProperties?: {
    readonly type?: string;
    readonly items?: { readonly type?: string };
  };
  /** Suggestion source for a scalar or array field's values (`toolkit_names`). */
  readonly enum_source?: string;
  /** Suggestion source for an object field's KEYS (`toolkit_names`). */
  readonly enum_source_keys?: string;
  /** Suggestion source for an object field's VALUES (`toolkit_tools`). */
  readonly enum_source_values?: string;
  /**
   * The FIELD-level twin of the section-level reason — unit A14's Features page.
   *
   * A section is the wrong granularity when one control inside a working section
   * has nothing behind it: `agent_publishing` enforces its block switch and its
   * whitelist for real and feeds its categories to the Agents Hub, and only
   * `publish_validation_rules` is inert. Withholding the whole section would
   * have taken three working controls away to disclose one broken one.
   *
   * The server refuses a write to such a field with a 400 naming it, so this is
   * presentation of a fact the endpoint enforces, never the enforcement itself.
   */
  readonly unavailable_reason?: string;
  /** Renders the field only while another field in the same section holds a value. */
  readonly visible_when?:
    | { readonly field: string; readonly value: unknown }
    | ReadonlyArray<{ readonly field: string; readonly value: unknown }>;
}

/**
 * One section of `GET /admin/plugin_config_schemas/administration`.
 *
 * `unavailable_reason` is the load-bearing member. The server decides which
 * sections this deployment can serve, because that is a fact about the
 * deployment and a page that decided it locally would drift from what the
 * endpoints actually do — the value endpoints answer 501 with this exact string.
 */
export interface AdminConfigSection {
  readonly id: string;
  readonly title: string;
  readonly description?: string;
  readonly fields?: readonly AdminConfigField[];
  readonly unavailable_reason?: string;
  /**
   * A server-stated fact about this deployment that the operator should see
   * above the form, e.g. how the cloud executor's storage encryption at rest
   * was established ("attested by operator" vs reported by the backend).
   * Not editable here: it reflects the deployment, not a stored value.
   */
  readonly notice?: string;
  /**
   * The dedicated surface that really holds this section's data, when one
   * exists — `'mcp_prebuilt_servers'` for the MCP catalogue.
   *
   * Server-declared, for the reason `page` is: a client that keyed a bespoke
   * editor on `id === 'mcp_servers'` would go on rendering it after the server
   * moved or withdrew the surface. Keying on the server's own word means the
   * server decides, and a client that does not recognise the value falls back
   * to rendering `unavailable_reason` — which is why that field STAYS set on a
   * managed section.
   */
  readonly managed_surface?: string;
  /**
   * Which admin page renders this section — `'features'`, or absent for
   * Configuration.
   *
   * Server-declared, and that is the point. The reference keeps this in TWO
   * client-side lists that must stay each other's exact complement:
   * `FeaturesPage.jsx` hardcodes six sections and three config-path prefixes,
   * and `ConfigurationPage.jsx` hardcodes `MOVED_TO_FEATURES` plus the same
   * three prefixes to subtract. A section added on the server would appear on
   * Configuration, or on both, depending on which list was updated. Declaring it
   * once, next to the fields, is the same correction #217 made when it moved
   * `service_descriptors` out of the client's section list.
   */
  readonly page?: string;
}

/** The value of `page` for the sections the Features page owns. */
export const ADMIN_CONFIG_PAGE_FEATURES = 'features';

/** The GET body. Not exported: no call site outside this module reads it. */
interface AdminConfigValues {
  readonly values: Readonly<Record<string, unknown>>;
}

/**
 * One query-key namespace, declared once.
 *
 * The save invalidates `adminConfigKeys.all`, so a key built ad hoc at a call
 * site would be a cache the write never refreshes — the read/write key-namespace
 * split that made saved data look absent in #132.
 */
const adminConfigKeys = {
  all: ['admin', 'configuration'] as const,
  schemas: () => ['admin', 'configuration', 'schemas'] as const,
  values: (sectionId: string) => ['admin', 'configuration', 'values', sectionId] as const,
};

/**
 * The server's own explanation of a refusal, when it gave one.
 *
 * It carries more weight on this page than on a listing: every one of the
 * server's refusals here is actionable and specific — a link scheme that would
 * execute in a reader's browser, a key the schema does not declare, a section
 * this deployment cannot serve — and collapsing them into "Failed to save" would
 * discard the only sentence that says which.
 *
 * A 401 does not arrive here (a 403 does, since issue 93):
 * `shared/api/http.ts` routes a 401 into the
 * single-flight re-auth path and reports `kind: 'auth'`, which carries no body.
 */
export function configFailureReason(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  if (failure.kind !== 'http') return undefined;
  const body = failure.body;
  if (typeof body !== 'object' || body === null) return undefined;
  const reason = (body as { error?: unknown }).error;
  return typeof reason === 'string' && reason !== '' ? reason : undefined;
}

/** The HTTP status of a failure, when it had one. Distinguishes 501 from 4xx. */
export function configFailureStatus(error: unknown): number | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  return failure.kind === 'http' ? failure.status : undefined;
}

/** `GET /admin/plugin_config_schemas/administration`. */
export function useAdminConfigSections(): UseQueryResult<readonly AdminConfigSection[], Error> {
  return useQuery({
    queryKey: adminConfigKeys.schemas(),
    queryFn: async (): Promise<readonly AdminConfigSection[]> => {
      // `eliteaFetch` resolves the transport envelope, not the body; peeling it
      // by hand at a call site is what R-A6 exists to prevent, and forgetting to
      // peel it at all is #132's silent empty state.
      const body = unwrapBody(await eliteaFetch<unknown>(SCHEMAS_URL)) as
        | { sections?: AdminConfigSection[] }
        | undefined;
      return body?.sections ?? [];
    },
  });
}

/**
 * `GET /admin/plugin_config_values/administration/{section}`.
 *
 * `enabled` is false for a section the server declared unavailable. Fetching it
 * anyway would spend a request to be told 501 — and, worse, would put a failed
 * query behind a pane that already knows why it is empty, so the page would show
 * a load error over an explanation.
 */
export function useAdminConfigValues(
  sectionId: string | undefined,
  enabled: boolean,
): UseQueryResult<Readonly<Record<string, unknown>>, Error> {
  return useQuery({
    queryKey: adminConfigKeys.values(sectionId ?? ''),
    enabled: enabled && sectionId !== undefined && sectionId !== '',
    queryFn: async (): Promise<Readonly<Record<string, unknown>>> => {
      const body = unwrapBody(await eliteaFetch<unknown>(sectionValuesUrl(sectionId ?? ''))) as
        | AdminConfigValues
        | undefined;
      return body?.values ?? {};
    },
  });
}

export interface AdminConfigSave {
  readonly sectionId: string;
  readonly values: Readonly<Record<string, unknown>>;
}

/**
 * `PUT /admin/plugin_config_values/administration/{section}`.
 *
 * Sends only the keys the operator changed. The server applies exactly what it
 * is given and leaves the rest alone, so saving one card cannot blank another —
 * and a smaller body is a smaller surface for the validator to refuse over a
 * field nobody touched.
 */
export function useSaveAdminConfigValues(): UseMutationResult<void, Error, AdminConfigSave> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ sectionId, values }: AdminConfigSave) => {
      await eliteaFetch<unknown>(sectionValuesUrl(sectionId), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ values }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminConfigKeys.all }),
  });
}

/* ── suggestions ─────────────────────────────────────────────────────────── */

/**
 * `GET /admin/plugin_config_suggestions/administration/{source}`.
 *
 * The pickers behind the Guardrails fields. The endpoint answers
 * `{values, labels}`; only `values` is read here, because for both toolkit
 * sources the identifier IS the display text and the server sends `labels`
 * empty rather than inventing prettified names.
 *
 * ## Why a failure is not surfaced as an error
 *
 * Every control that uses these is FREE-SOLO: the operator can type a toolkit or
 * tool name the registry does not know, and must be able to — `blocked_tools`
 * legitimately names types the pinned snapshot does not declare, and
 * `sensitive_tools` uses the `*` wildcard, which is not a toolkit name at all.
 * So a 501 or a network fault degrades to "no suggestions", never to a blocked
 * field. `retry: false` keeps a deployment whose registry is unwired from
 * re-asking on every keystroke-driven remount.
 */
function suggestionsUrl(source: string, toolkit?: string): string {
  const base = `/admin/plugin_config_suggestions/${ADMIN_MODE}/${encodeURIComponent(source)}`;
  return toolkit === undefined ? base : `${base}?toolkit=${encodeURIComponent(toolkit)}`;
}

async function fetchSuggestions(source: string, toolkit?: string): Promise<readonly string[]> {
  const body = unwrapBody(await eliteaFetch<unknown>(suggestionsUrl(source, toolkit))) as
    | { values?: unknown }
    | undefined;
  const values = body?.values;
  if (!Array.isArray(values)) return [];
  return values.filter((entry): entry is string => typeof entry === 'string');
}

/** Suggestions for a source that takes no parameter (`toolkit_names`, `projects`). */
export function useConfigSuggestions(source: string | undefined): readonly string[] {
  const query = useQuery({
    queryKey: ['admin', 'configuration', 'suggestions', source ?? ''],
    enabled: source !== undefined && source !== '',
    retry: false,
    staleTime: 5 * 60 * 1000,
    queryFn: async () => fetchSuggestions(source ?? ''),
  });
  return query.data ?? [];
}

/**
 * Suggestions for `toolkit_tools`, which is scoped to one toolkit.
 *
 * Disabled while the toolkit is blank or is the `*` wildcard: there is no such
 * toolkit to enumerate tools for, and asking would spend a request to be told an
 * empty list. The wildcard row therefore offers no tool suggestions, which is
 * honest — a wildcard entry applies to every toolkit, so no single toolkit's
 * tool list is the right one to offer.
 */
export function useToolkitToolSuggestions(
  source: string | undefined,
  toolkit: string,
): readonly string[] {
  const trimmed = toolkit.trim();
  const query = useQuery({
    queryKey: ['admin', 'configuration', 'suggestions', source ?? '', trimmed],
    enabled: source !== undefined && source !== '' && trimmed !== '' && trimmed !== '*',
    retry: false,
    staleTime: 5 * 60 * 1000,
    queryFn: async () => fetchSuggestions(source ?? '', trimmed),
  });
  return query.data ?? [];
}
