/**
 * REST client for the TYPED identity provider definitions.
 *
 * A provider is the authored answer to "how does this deployment federate a
 * login". It is what the admin Configuration page's "Authentication" section is
 * about, and it is NOT stored as plugin configuration: an OIDC client secret
 * and a SAML service-provider key are credentials, and the plugin-config value
 * endpoints keep their values in plaintext rows readable by everyone who can
 * open that page. So the definition has its own surface, and this module speaks
 * to it.
 *
 * Wire contract: `services/elitea-main/internal/api/v2/admin/identity_providers.go`.
 *
 *   GET    /admin/identity_providers/administration
 *   PUT    /admin/identity_providers/administration/{key}
 *   DELETE /admin/identity_providers/administration/{key}
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as
 * `./adminMcpServersApi.ts`, and reusing its failure readers rather than
 * restating them.
 *
 * ## The secret is never in hand here
 *
 * A read returns `secret` as a MASK when one is sealed and omits it entirely
 * when none is. This module never holds a plaintext secret except the one the
 * operator has just typed, on its way into a save.
 *
 * ## `secret` is tri-state on save, and that is load-bearing
 *
 * Absent, empty string, and a value mean three different things — leave the
 * sealed secret alone, clear it, and re-seal it. The dialog cannot echo the
 * current secret (it never receives it), so a save that always sent the field
 * would erase the credential every time an operator edited a URL. On this
 * surface that would take the deployment's single sign-on down.
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

/** Federation is a deployment fact; there is no project-scoped view of it. */
const ADMIN_MODE = 'administration';

const PROVIDERS_URL = `/admin/identity_providers/${ADMIN_MODE}`;

function providerUrl(key: string): string {
  return `${PROVIDERS_URL}/${encodeURIComponent(key)}`;
}

/**
 * The `managed_surface` value the server declares on the section this editor
 * owns. Exported so the page's registry keys on the SERVER's word rather than
 * on a section id this app chose to recognise.
 */
export const IDENTITY_PROVIDERS_MANAGED_SURFACE = 'identity_providers';

/** The protocols this service has a login path for. */
export type AdminIdentityProviderKind = 'oidc' | 'saml';

/** The OpenID Connect document. Never carries the client secret. */
export interface AdminOidcDocument {
  readonly issuer: string;
  readonly client_id: string;
  readonly redirect_uri: string;
  readonly scopes?: readonly string[];
  readonly require_email_verified?: boolean;
  /** Email domains the sign-in page routes to this provider. Lower case. */
  readonly login_domains?: readonly string[];
  /** Whether a first sign-in here may adopt a SCIM-provisioned account. */
  readonly adopt_scim_users?: boolean;
}

/** The SAML 2.0 document. Certificates here are the identity provider's PUBLIC ones. */
export interface AdminSamlDocument {
  readonly idp_entity_id: string;
  readonly idp_sso_url: string;
  readonly idp_slo_url?: string;
  readonly idp_certificates?: readonly string[];
  readonly sp_entity_id: string;
  readonly acs_url: string;
  readonly name_id_format?: string;
  readonly email_attribute?: string;
  readonly name_attribute?: string;
  readonly sign_authn_requests?: boolean;
  readonly sp_certificate?: string;
  readonly clock_skew_seconds?: number;
  /** Email domains the sign-in page routes to this provider. Lower case. */
  readonly login_domains?: readonly string[];
  /** Whether a first sign-in here may adopt a SCIM-provisioned account. */
  readonly adopt_scim_users?: boolean;
}

/** One authored definition, as the server renders it. */
export interface AdminIdentityProvider {
  readonly key: string;
  readonly kind: AdminIdentityProviderKind;
  readonly display_name: string;
  readonly enabled: boolean;
  readonly revision: number;
  /** The mask when a secret is sealed; absent when none is. Never the value. */
  readonly secret?: string;
  readonly updated_at?: string;
  readonly oidc?: AdminOidcDocument;
  readonly saml?: AdminSamlDocument;
}

/**
 * One query-key namespace, declared once.
 *
 * Every mutation invalidates `adminIdentityProviderKeys.all`. A key built ad hoc
 * at a call site would be a cache the writes never refresh — the read/write
 * key-namespace split that made saved data look absent in #132.
 */
const adminIdentityProviderKeys = {
  all: ['admin', 'identityProviders'] as const,
  list: () => ['admin', 'identityProviders', 'list'] as const,
};

/** `GET /admin/identity_providers/administration`. */
export function useAdminIdentityProviders(): UseQueryResult<
  readonly AdminIdentityProvider[],
  Error
> {
  return useQuery({
    queryKey: adminIdentityProviderKeys.list(),
    queryFn: async (): Promise<readonly AdminIdentityProvider[]> => {
      // `eliteaFetch` resolves the transport envelope, not the body. Forgetting
      // to peel the body is #132's silent empty state.
      const body = unwrapBody(await eliteaFetch<unknown>(PROVIDERS_URL)) as
        | { providers?: AdminIdentityProvider[] }
        | undefined;
      return body?.providers ?? [];
    },
  });
}

/** What the dialog collects. `secret` absent ⇒ leave the sealed one alone. */
export interface AdminIdentityProviderDraft {
  readonly key: string;
  readonly kind: AdminIdentityProviderKind;
  readonly displayName: string;
  readonly enabled: boolean;
  /** `undefined` ⇒ unchanged. `''` ⇒ clear it. A value ⇒ re-seal it. */
  readonly secret: string | undefined;
  readonly oidc?: AdminOidcDocument;
  readonly saml?: AdminSamlDocument;
}

/** `PUT /admin/identity_providers/administration/{key}`. */
export function useSaveAdminIdentityProvider(): UseMutationResult<
  void,
  Error,
  AdminIdentityProviderDraft
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (draft: AdminIdentityProviderDraft) => {
      const body: Record<string, unknown> = {
        kind: draft.kind,
        display_name: draft.displayName,
        enabled: draft.enabled,
      };
      // Exactly one document is sent, and it is the one the kind names. Sending
      // both would ask the server to store a definition of one protocol
      // carrying the other's values.
      if (draft.kind === 'oidc') {
        body['oidc'] = draft.oidc;
      } else {
        body['saml'] = draft.saml;
      }
      // Sent ONLY when the operator changed it. See the tri-state note above:
      // an always-sent field would clear the credential on every unrelated edit.
      if (draft.secret !== undefined) {
        body['secret'] = draft.secret;
      }
      await eliteaFetch<unknown>(providerUrl(draft.key), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminIdentityProviderKeys.all }),
  });
}

/** `DELETE /admin/identity_providers/administration/{key}`. */
export function useDeleteAdminIdentityProvider(): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (key: string) => {
      await eliteaFetch<unknown>(providerUrl(key), { method: 'DELETE' });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminIdentityProviderKeys.all }),
  });
}
