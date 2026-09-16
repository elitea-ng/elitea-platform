/**
 * REST client for the PRE-BUILT MCP server catalogue.
 *
 * The catalogue is the platform-wide list of MCP servers an operator offers as
 * ready-made toolkits. It is what the admin Configuration page's "MCP Servers"
 * section is about, and it is NOT stored as plugin configuration: an entry
 * carries a client secret, and the plugin-config value endpoints keep their
 * values in plaintext rows readable by everyone who can open that page. So the
 * catalogue has its own surface, and this module speaks to it.
 *
 * Wire contract: `services/elitea-main/internal/api/v2/admin/mcp_prebuilt.go`.
 *
 *   GET    /admin/mcp_prebuilt_servers/administration
 *   PUT    /admin/mcp_prebuilt_servers/administration/{key}
 *   DELETE /admin/mcp_prebuilt_servers/administration/{key}
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as
 * `./adminConfigurationApi.ts`, and reusing its failure readers rather than
 * restating them.
 *
 * ## The secret is never in hand here
 *
 * A read returns `client_secret` as a MASK when one is set and omits it
 * entirely when none is. This module never holds a plaintext secret except the
 * one the operator has just typed, on its way into a save. That is the whole
 * reason the catalogue is not a plugin-config section.
 *
 * ## `client_secret` is tri-state on save, and that is load-bearing
 *
 * Absent, empty string, and a value mean three different things — leave the
 * sealed secret alone, clear it, and re-seal it. The dialog cannot echo the
 * current secret (it never receives it), so a save that always sent the field
 * would erase the credential every time an operator edited a URL. `saveServer`
 * therefore omits the key unless the operator actually changed it.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type QueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { getListToolkitsQueryKey } from '@/shared/api/generated/toolkits/toolkits';
import { unwrapBody } from '@/shared/api/unwrap';

/** The catalogue is platform-wide; there is no project-scoped view of it. */
const ADMIN_MODE = 'administration';

const CATALOGUE_URL = `/admin/mcp_prebuilt_servers/${ADMIN_MODE}`;

function serverUrl(key: string): string {
  return `${CATALOGUE_URL}/${encodeURIComponent(key)}`;
}

/**
 * The `managed_surface` value the server declares on the section this editor
 * owns. Exported so the page's registry keys on the SERVER's word rather than
 * on a section id this app chose to recognise.
 */
export const MCP_SERVERS_MANAGED_SURFACE = 'mcp_prebuilt_servers';

/** One catalogue entry, as the server renders it. Never carries a plaintext secret. */
export interface AdminMcpServer {
  readonly key: string;
  readonly display_name: string;
  readonly url: string;
  readonly base_url: string;
  readonly client_id: string;
  /** The mask when a secret is set; absent when none is. Never the value. */
  readonly client_secret?: string;
  readonly timeout: number;
  readonly headers: Readonly<Record<string, string>>;
  /** Project-owned fields rendered on each ready-made toolkit form. */
  readonly config_schema: Readonly<Record<string, unknown>>;
  readonly enabled: boolean;
}

/**
 * One query-key namespace, declared once.
 *
 * Every mutation invalidates `adminMcpServerKeys.all`. A key built ad hoc at a
 * call site would be a cache the writes never refresh — the read/write
 * key-namespace split that made saved data look absent in #132.
 */
const adminMcpServerKeys = {
  all: ['admin', 'mcpPrebuiltServers'] as const,
  list: () => ['admin', 'mcpPrebuiltServers', 'list'] as const,
};

/** All project-scoped toolkit type catalogues share this generated path prefix. */
const TOOLKIT_TYPE_CATALOGUE_PREFIX = getListToolkitsQueryKey('')[0];

function isToolkitTypeCatalogueQuery(query: { readonly queryKey: readonly unknown[] }): boolean {
  const first = query.queryKey[0];
  return typeof first === 'string' && first.startsWith(TOOLKIT_TYPE_CATALOGUE_PREFIX);
}

async function invalidateCatalogueQueries(queryClient: QueryClient): Promise<void> {
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: adminMcpServerKeys.all }),
    queryClient.invalidateQueries({ predicate: isToolkitTypeCatalogueQuery }),
  ]);
}

/** `GET /admin/mcp_prebuilt_servers/administration`. */
export function useAdminMcpServers(): UseQueryResult<readonly AdminMcpServer[], Error> {
  return useQuery({
    queryKey: adminMcpServerKeys.list(),
    queryFn: async (): Promise<readonly AdminMcpServer[]> => {
      // `eliteaFetch` resolves the transport envelope, not the body. Forgetting
      // to peel the body is #132's silent empty state.
      const body = unwrapBody(await eliteaFetch<unknown>(CATALOGUE_URL)) as
        | { servers?: AdminMcpServer[] }
        | undefined;
      return body?.servers ?? [];
    },
  });
}

/** What the dialog collects. `clientSecret` absent ⇒ leave the sealed one alone. */
export interface AdminMcpServerDraft {
  readonly key: string;
  readonly displayName: string;
  readonly url: string;
  readonly baseUrl: string;
  readonly clientId: string;
  /** `undefined` ⇒ unchanged. `''` ⇒ clear it. A value ⇒ re-seal it. */
  readonly clientSecret: string | undefined;
  readonly timeout: number;
  readonly headers: Readonly<Record<string, string>>;
  readonly configSchema: Readonly<Record<string, unknown>>;
  readonly enabled: boolean;
}

/** `PUT /admin/mcp_prebuilt_servers/administration/{key}`. */
export function useSaveAdminMcpServer(): UseMutationResult<void, Error, AdminMcpServerDraft> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (draft: AdminMcpServerDraft) => {
      const body: Record<string, unknown> = {
        display_name: draft.displayName,
        url: draft.url,
        base_url: draft.baseUrl,
        client_id: draft.clientId,
        timeout: draft.timeout,
        headers: draft.headers,
        config_schema: draft.configSchema,
        enabled: draft.enabled,
        // Declared explicitly so the server's stdio refusal is a contract this
        // client is on the right side of, rather than a default it relies on.
        transport: 'http',
      };
      // Sent ONLY when the operator changed it. See the tri-state note above:
      // an always-sent field would clear the credential on every unrelated edit.
      if (draft.clientSecret !== undefined) {
        body['client_secret'] = draft.clientSecret;
      }
      await eliteaFetch<unknown>(serverUrl(draft.key), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
    },
    onSuccess: () => invalidateCatalogueQueries(queryClient),
  });
}

/** `DELETE /admin/mcp_prebuilt_servers/administration/{key}`. */
export function useDeleteAdminMcpServer(): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (key: string) => {
      await eliteaFetch<unknown>(serverUrl(key), { method: 'DELETE' });
    },
    onSuccess: () => invalidateCatalogueQueries(queryClient),
  });
}
