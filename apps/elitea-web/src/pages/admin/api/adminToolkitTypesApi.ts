/**
 * REST client for the admin TOOLKIT TYPES surface (shared migration 0114).
 *
 * Five calls, matching the five routes in
 * `services/elitea-main/internal/api/v2/admin/toolkit_types.go`: the listing,
 * one decision, the two per-project exception verbs, and a bulk apply.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as
 * `./adminServiceDescriptorsApi.ts`.
 *
 * ## `availability: 'default'` is the revert, not a stored word
 *
 * The server DELETES the row for it, and the listing then reports the type as
 * `default` again. It is spelled as a value of the same field rather than as a
 * separate call because the page offers four choices in one control.
 *
 * ## What is reused
 *
 * `unwrapBody` and the two failure readers from `./adminConfigurationApi`. The
 * envelope trap is #132: `eliteaFetch` resolves the transport envelope, not the
 * body, so reading `types` off the envelope renders an empty page against a
 * perfectly good 200.
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

import { configFailureReason, configFailureStatus } from './adminConfigurationApi';

/** The only mode the server registers on this path; a static segment on both sides. */
const ADMIN_MODE = 'administration';

const TOOLKIT_TYPES_URL = `/admin/toolkit_types/${ADMIN_MODE}`;

/** The stored decision, or `default` when the deployment recorded none. */
export type ToolkitTypeAvailability = 'default' | 'enabled' | 'disabled' | 'restricted';

/** What a per-project exception may say. `restricted` is meaningless on one. */
export type ToolkitTypeGrantAvailability = 'enabled' | 'disabled';

interface AdminToolkitTypeGrant {
  readonly project_id: number;
  readonly availability: ToolkitTypeGrantAvailability;
  readonly reason: string;
  readonly granted_by: string;
  readonly granted_at: string;
}

/**
 * The worker-support verdict, which is ADVISORY.
 *
 * `unverified` is not a claim that the type fails. It reports that neither the
 * pinned Python worker image nor the Rust worker is KNOWN to carry it. The page
 * shows `reason` beside it so an operator does not read the chip as a refusal.
 */
interface AdminToolkitTypeCapability {
  readonly verdict: 'supported' | 'unverified';
  readonly python: boolean;
  readonly rust: boolean;
  readonly reason: string;
}

export interface AdminToolkitType {
  readonly type: string;
  readonly label: string;
  readonly category: string;
  readonly availability: ToolkitTypeAvailability;
  readonly reason: string;
  readonly decided_by: string;
  readonly decided_at: string;
  /** What a project with NO exception sees. A restricted type is false here. */
  readonly available: boolean;
  readonly source: 'default' | 'deployment' | 'project';
  /**
   * Whether this deployment's registry still enumerates the type. A decision
   * for a type the registry dropped is still listed, and still reversible.
   */
  readonly registered: boolean;
  readonly capability: AdminToolkitTypeCapability;
  readonly project_grants: readonly AdminToolkitTypeGrant[];
}

interface AdminToolkitTypeListBody {
  readonly types?: readonly AdminToolkitType[];
  readonly categories?: readonly string[];
  readonly total?: number;
  readonly registry_available?: boolean;
}

export interface AdminToolkitTypeListing {
  readonly types: readonly AdminToolkitType[];
  readonly categories: readonly string[];
  /**
   * `false` means this deployment cannot enumerate its toolkit registry. The
   * page says so rather than rendering an empty grid: zero toolkit types is not
   * a state this platform can be in, so an empty list would be a false
   * statement about the deployment.
   */
  readonly registryAvailable: boolean;
}

/**
 * One query-key namespace, declared once — the read/write split that made saved
 * data look absent in #132. Not exported: the test pins the literal key against
 * the query cache, which is a stronger pin than importing the builder.
 */
const adminToolkitTypeKeys = {
  list: () => ['admin', 'toolkit-types', 'list'] as const,
};

/** `GET /admin/toolkit_types/administration`. */
export function useAdminToolkitTypes(): UseQueryResult<AdminToolkitTypeListing, Error> {
  return useQuery({
    queryKey: adminToolkitTypeKeys.list(),
    retry: false,
    queryFn: async (): Promise<AdminToolkitTypeListing> => {
      const body = unwrapBody(await eliteaFetch<unknown>(TOOLKIT_TYPES_URL)) as
        | AdminToolkitTypeListBody
        | undefined;
      return {
        types: body?.types ?? [],
        categories: body?.categories ?? [],
        // `undefined` is treated as UNAVAILABLE rather than available. An older
        // build that does not send the field cannot promise the registry
        // answered, and a reassuring default here would hide exactly the case
        // the field exists to report.
        registryAvailable: body?.registry_available === true,
      };
    },
  });
}

export interface SaveToolkitTypePolicy {
  readonly type: string;
  readonly availability: ToolkitTypeAvailability;
  /** Required by the server for every value except `default`. */
  readonly reason: string;
}

function typeUrl(type: string): string {
  return `${TOOLKIT_TYPES_URL}/${encodeURIComponent(type)}`;
}

function projectUrl(type: string, projectId: number): string {
  return `${typeUrl(type)}/projects/${projectId}`;
}

/** `PUT /admin/toolkit_types/administration/{type}`. */
export function useSaveToolkitTypePolicy(): UseMutationResult<void, Error, SaveToolkitTypePolicy> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: SaveToolkitTypePolicy) => {
      await eliteaFetch<unknown>(typeUrl(input.type), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ availability: input.availability, reason: input.reason }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminToolkitTypeKeys.list() }),
  });
}

export interface SaveToolkitTypeGrant {
  readonly type: string;
  readonly projectId: number;
  readonly availability: ToolkitTypeGrantAvailability;
  readonly reason: string;
}

/** `PUT /admin/toolkit_types/administration/{type}/projects/{projectID}`. */
export function useSaveToolkitTypeGrant(): UseMutationResult<void, Error, SaveToolkitTypeGrant> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: SaveToolkitTypeGrant) => {
      await eliteaFetch<unknown>(projectUrl(input.type, input.projectId), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ availability: input.availability, reason: input.reason }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminToolkitTypeKeys.list() }),
  });
}

export interface RevokeToolkitTypeGrant {
  readonly type: string;
  readonly projectId: number;
}

/** `DELETE /admin/toolkit_types/administration/{type}/projects/{projectID}`. */
export function useRevokeToolkitTypeGrant(): UseMutationResult<void, Error, RevokeToolkitTypeGrant> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: RevokeToolkitTypeGrant) => {
      await eliteaFetch<unknown>(projectUrl(input.type, input.projectId), { method: 'DELETE' });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminToolkitTypeKeys.list() }),
  });
}

interface BulkToolkitTypePolicy {
  readonly types: readonly string[];
  readonly availability: ToolkitTypeAvailability;
  readonly reason: string;
}

/**
 * `POST /admin/toolkit_types/administration/bulk`.
 *
 * The server answers 400 when NOTHING landed, so a bulk apply that changed
 * nothing surfaces as a failure rather than as a silent success.
 */
export function useBulkToolkitTypePolicy(): UseMutationResult<void, Error, BulkToolkitTypePolicy> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: BulkToolkitTypePolicy) => {
      await eliteaFetch<unknown>(`${TOOLKIT_TYPES_URL}/bulk`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          types: input.types,
          availability: input.availability,
          reason: input.reason,
        }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminToolkitTypeKeys.list() }),
  });
}

/**
 * The server's own explanation of a refusal, and its status. Re-exported rather
 * than re-derived: two readers of `EliteaApiError`'s failure shape would be two
 * things to keep in step.
 */
export { configFailureReason as toolkitTypeFailureReason, configFailureStatus as toolkitTypeFailureStatus };
