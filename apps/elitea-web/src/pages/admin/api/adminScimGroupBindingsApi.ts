/**
 * REST client for the SCIM GROUP BINDINGS.
 *
 * A binding is the authored half of a SCIM group push. A SCIM group carries a
 * name and a member list; membership on this platform is always (user, project,
 * role). The binding says which project and which role, so the identity
 * provider supplies only the members — it cannot choose a project, cannot
 * choose a role, and cannot create or delete either.
 *
 * Wire contract: `services/elitea-main/internal/api/v2/admin/scim_group_bindings.go`.
 *
 *   GET    /admin/scim_group_bindings/administration
 *   GET    /admin/scim_group_bindings/administration/project_roles/{projectID}
 *   POST   /admin/scim_group_bindings/administration
 *   PUT    /admin/scim_group_bindings/administration/{id}
 *   DELETE /admin/scim_group_bindings/administration/{id}
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the shape of `./adminIdentityProvidersApi.ts`
 * and reusing its failure reader rather than restating it.
 *
 * ## `id` is a string here, as it is on the SCIM resource
 *
 * The server renders it as a string on both surfaces, so this screen and an
 * identity provider address the same binding the same way. It is never parsed
 * into a number: a JSON number round-trips through a float.
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

import { reasonFields, reasonHeaders, type AdminReason } from "../adminDangerousAction";

/** A binding is a deployment fact; there is no project-scoped view of it. */
const ADMIN_MODE = "administration";

const BINDINGS_URL = `/admin/scim_group_bindings/${ADMIN_MODE}`;

function bindingUrl(id: string): string {
  return `${BINDINGS_URL}/${encodeURIComponent(id)}`;
}

/**
 * One account a group put into the project.
 *
 * `granted` is the difference between a membership the push CREATED and one it
 * merely found. A member with `granted: false` was already in the project when
 * the group first pushed — they keep their access when the group lets go, and
 * the table says so, because otherwise an operator would expect removing them
 * from the group to remove them from the project.
 *
 * NOT exported: it is reached through `AdminScimGroupBinding.members`, and
 * nothing names it on its own. An exported type with no importer fails the dead
 * code gate, which is what that gate is for — an unused export is a contract
 * this app does not actually have.
 */
interface AdminScimGroupMember {
  readonly user_id: number;
  readonly user_name: string;
  readonly display_name?: string;
  readonly granted: boolean;
}

/** One page of bindings, with the total the page was taken from. */
export interface AdminScimGroupBindingPage {
  readonly bindings: readonly AdminScimGroupBinding[];
  readonly total: number;
  readonly limit: number;
  readonly offset: number;
}

/** One authored binding, as the server renders it. */
export interface AdminScimGroupBinding {
  readonly id: string;
  readonly display_name: string;
  /** The identity provider's own identifier. Absent until a push has arrived. */
  readonly external_id?: string;
  readonly project_id: number;
  readonly project_name?: string;
  readonly role_name: string;
  readonly members: readonly AdminScimGroupMember[];
  readonly updated_at?: string;
}

/** What the dialog collects. */
export interface AdminScimGroupBindingDraft {
  /** Absent for a new binding. */
  readonly id?: string;
  readonly displayName: string;
  readonly projectId: number;
  readonly roleName: string;
}

/**
 * One query-key namespace, declared once. Every mutation invalidates `all`; a
 * key built at a call site would be a cache the writes never refresh.
 */
const adminScimGroupBindingKeys = {
  all: ["admin", "scimGroupBindings"] as const,
  list: (offset: number) =>
    ["admin", "scimGroupBindings", "list", offset] as const,
  projectRoles: (projectId: number) =>
    ["admin", "scimGroupBindings", "projectRoles", projectId] as const,
};

/** The page this screen reads. The server bounds a larger request to 500. */
export const SCIM_GROUP_BINDING_PAGE_SIZE = 100;

/**
 * `GET /admin/scim_group_bindings/administration` — one page.
 *
 * The TOTAL is carried through, not discarded. A screen that renders a page and
 * says nothing about the rest is a screen on which a binding past that page
 * cannot be found, edited or removed — and an operator who cannot find one
 * authors a duplicate that the unique group name then refuses.
 */
export function useAdminScimGroupBindings(
  offset = 0,
): UseQueryResult<AdminScimGroupBindingPage, Error> {
  return useQuery({
    queryKey: adminScimGroupBindingKeys.list(offset),
    queryFn: async (): Promise<AdminScimGroupBindingPage> => {
      // `eliteaFetch` resolves the transport envelope, not the body. Forgetting
      // to peel the body is the silent empty state #132 shipped.
      const body = unwrapBody(
        await eliteaFetch<unknown>(
          `${BINDINGS_URL}?limit=${String(SCIM_GROUP_BINDING_PAGE_SIZE)}&offset=${String(offset)}`,
        ),
      ) as
        | {
            bindings?: AdminScimGroupBinding[];
            total?: number;
            limit?: number;
            offset?: number;
          }
        | undefined;
      const bindings = body?.bindings ?? [];
      return {
        bindings,
        // The total falls back to what ARRIVED rather than to zero: a missing
        // total must not read as "there is nothing else".
        total: body?.total ?? bindings.length,
        limit: body?.limit ?? SCIM_GROUP_BINDING_PAGE_SIZE,
        offset: body?.offset ?? offset,
      };
    },
  });
}

/**
 * `GET /admin/scim_group_bindings/administration/project_roles/{projectID}` —
 * the roles the chosen project REALLY has.
 *
 * NOT `useProjectRoles` from `./adminProjectsApi`. That reads
 * `/admin/roles/{mode}/{projectID}`, whose handler answers a hardcoded
 * admin/editor/viewer for a project carrying no role rows
 * (`internal/api/v2/eliteacore/handler.go`). A picker fed by that offers a role
 * the project does not have, the operator picks it, and the save is refused by
 * a value the control itself supplied.
 *
 * Disabled until a project is chosen, so the dialog does not ask about project
 * zero on its way to asking about a real one.
 */
export function useAdminScimGroupProjectRoles(
  projectId: number,
): UseQueryResult<readonly string[], Error> {
  return useQuery({
    queryKey: adminScimGroupBindingKeys.projectRoles(projectId),
    enabled: Number.isInteger(projectId) && projectId > 0,
    queryFn: async (): Promise<readonly string[]> => {
      const body = unwrapBody(
        await eliteaFetch<unknown>(
          `${BINDINGS_URL}/project_roles/${String(projectId)}`,
        ),
      ) as { roles?: string[] } | undefined;
      return body?.roles ?? [];
    },
  });
}

/**
 * `POST` for a new binding, `PUT /{id}` for an existing one.
 *
 * A save that changes the project or the role MOVES the access the group
 * granted: the server revokes under the old pair and re-grants under the new
 * one in a single transaction. The dialog says so before the operator saves.
 */
export function useSaveAdminScimGroupBinding(): UseMutationResult<
  void,
  Error,
  { readonly draft: AdminScimGroupBindingDraft; readonly reason: AdminReason }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ draft, reason }) => {
      const body = {
        display_name: draft.displayName,
        project_id: draft.projectId,
        role_name: draft.roleName,
        ...reasonFields(
          draft.id === undefined
            ? "POST /api/v2/admin/scim_group_bindings/administration"
            : "PUT /api/v2/admin/scim_group_bindings/administration/{id}",
          reason,
        ),
      };
      await eliteaFetch<unknown>(
        draft.id === undefined ? BINDINGS_URL : bindingUrl(draft.id),
        {
          method: draft.id === undefined ? "POST" : "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        },
      );
    },
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: adminScimGroupBindingKeys.all,
      }),
  });
}

/**
 * `DELETE /admin/scim_group_bindings/administration/{id}`.
 *
 * It withdraws every membership the group granted and removes the binding. It
 * does NOT delete the project, and it does not touch a member somebody added by
 * hand.
 */
export function useDeleteAdminScimGroupBinding(): UseMutationResult<
  void,
  Error,
  { readonly id: string; readonly reason: AdminReason }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ id, reason }) => {
      await eliteaFetch<unknown>(bindingUrl(id), {
        method: "DELETE",
        ...reasonHeaders("DELETE /api/v2/admin/scim_group_bindings/administration/{id}", reason),
      });
    },
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: adminScimGroupBindingKeys.all,
      }),
  });
}
