/**
 * REST client for the dedicated SCIM CLIENT credentials.
 *
 * SCIM 2.0 (`/api/v2/scim/v2`) authenticates an identity provider with a
 * credential minted for that purpose only — a personal access token is no
 * longer accepted. Two methods mirror Microsoft Entra ID provisioning:
 *
 *  - `bearer`: one long-lived secret token, sent as `Authorization: Bearer`.
 *  - `client_credentials`: a client id + secret exchanged at
 *    `/api/v2/scim/oauth/token` for a short-lived access token.
 *
 *   GET    /admin/scim_clients/administration
 *   POST   /admin/scim_clients/administration
 *   POST   /admin/scim_clients/administration/{id}/rotate
 *   POST   /admin/scim_clients/administration/{id}/revoke
 *   DELETE /admin/scim_clients/administration/{id}
 *
 * ## The secret never enters the query cache
 *
 * A create or a rotate answers the plaintext secret ONCE. The mutations return
 * it to the caller, which holds it in component state for the reveal dialog and
 * clears it on close. The list is INVALIDATED, never written from the response:
 * a cache entry would keep the secret alive after the dialog closed, and a
 * devtools panel would show it.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";

import { eliteaFetch } from "@/shared/api/generated/mutator";
import { unwrapBody } from "@/shared/api/unwrap";

const CLIENTS_URL = "/admin/scim_clients/administration";

/** Used until the listing has loaded; the server answers the same values. */
export const SCIM_BASE_PATH = "/api/v2/scim/v2";
export const SCIM_TOKEN_ENDPOINT_PATH = "/api/v2/scim/oauth/token";

function clientUrl(id: string, action?: "rotate" | "revoke"): string {
  const base = `${CLIENTS_URL}/${encodeURIComponent(id)}`;
  return action === undefined ? base : `${base}/${action}`;
}

export type ScimClientAuthMethod = "bearer" | "client_credentials";

/** One SCIM client as the server renders it. It never carries the secret. */
export interface AdminScimClient {
  readonly id: string;
  readonly name: string;
  readonly auth_method: ScimClientAuthMethod;
  readonly client_id?: string;
  /** The last four characters of the secret, so a row can be told apart. */
  readonly secret_hint: string;
  readonly status: "active" | "revoked" | "expired";
  readonly created_by?: number | null;
  readonly created_by_name?: string;
  readonly created_at: string;
  readonly last_used_at?: string | null;
  readonly rotated_at?: string | null;
  readonly revoked_at?: string | null;
  /** After this instant the client stops authenticating. Absent: no expiry. */
  readonly expires_at?: string | null;
}

interface AdminScimClientList {
  readonly clients: readonly AdminScimClient[];
  readonly total: number;
  readonly scimBasePath: string;
  readonly tokenEndpointPath: string;
}

/** What a create or a rotate answers: shown once, then dropped. */
export interface AdminScimClientSecret {
  readonly client: AdminScimClient;
  readonly secret: string;
  readonly clientId?: string | undefined;
}

const adminScimClientKeys = {
  all: ["admin", "scimClients"] as const,
  list: () => ["admin", "scimClients", "list"] as const,
};

/** `GET /admin/scim_clients/administration`. */
export function useAdminScimClients(): UseQueryResult<
  AdminScimClientList,
  Error
> {
  return useQuery({
    queryKey: adminScimClientKeys.list(),
    queryFn: async (): Promise<AdminScimClientList> => {
      const body = unwrapBody(await eliteaFetch<unknown>(CLIENTS_URL)) as
        | {
            clients?: AdminScimClient[];
            total?: number;
            scim_base_path?: string;
            token_endpoint_path?: string;
          }
        | undefined;
      const clients = body?.clients ?? [];
      return {
        clients,
        total: body?.total ?? clients.length,
        scimBasePath: body?.scim_base_path ?? SCIM_BASE_PATH,
        tokenEndpointPath: body?.token_endpoint_path ?? SCIM_TOKEN_ENDPOINT_PATH,
      };
    },
  });
}

function readSecret(raw: unknown): AdminScimClientSecret {
  const body = unwrapBody(raw) as {
    client: AdminScimClient;
    secret: string;
    client_id?: string;
  };
  return {
    client: body.client,
    secret: body.secret,
    clientId: body.client_id ?? body.client.client_id,
  };
}

export interface AdminScimClientDraft {
  readonly name: string;
  readonly authMethod: ScimClientAuthMethod;
  /** RFC 3339 instant; omitted for a client with no expiry. */
  readonly expiresAt?: string | undefined;
}

/** `POST /admin/scim_clients/administration` — answers the secret once. */
export function useCreateAdminScimClient(): UseMutationResult<
  AdminScimClientSecret,
  Error,
  AdminScimClientDraft
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (draft: AdminScimClientDraft) =>
      readSecret(
        await eliteaFetch<unknown>(CLIENTS_URL, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            name: draft.name,
            auth_method: draft.authMethod,
            ...(draft.expiresAt === undefined
              ? {}
              : { expires_at: draft.expiresAt }),
          }),
        }),
      ),
    // The result holds the plaintext secret: let the mutation cache drop it as
    // soon as the editor resets the mutation.
    gcTime: 0,
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: adminScimClientKeys.all }),
  });
}

/** `POST /{id}/rotate` — the old secret stops working immediately. */
export function useRotateAdminScimClient(): UseMutationResult<
  AdminScimClientSecret,
  Error,
  string
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) =>
      readSecret(
        await eliteaFetch<unknown>(clientUrl(id, "rotate"), {
          method: "POST",
        }),
      ),
    gcTime: 0,
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: adminScimClientKeys.all }),
  });
}

/** `POST /{id}/revoke` — the row stays listed as revoked. */
export function useRevokeAdminScimClient(): UseMutationResult<
  void,
  Error,
  string
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await eliteaFetch<unknown>(clientUrl(id, "revoke"), { method: "POST" });
    },
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: adminScimClientKeys.all }),
  });
}

/** `DELETE /{id}` — removes the record. */
export function useDeleteAdminScimClient(): UseMutationResult<
  void,
  Error,
  string
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await eliteaFetch<unknown>(clientUrl(id), { method: "DELETE" });
    },
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: adminScimClientKeys.all }),
  });
}
