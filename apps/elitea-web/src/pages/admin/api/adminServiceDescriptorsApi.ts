/**
 * REST client for the admin SERVICE DESCRIPTORS surface — unit A14, issue #200.
 *
 * THREE calls: the listing, activate and deactivate.
 *
 * ## The "answers 501" prose that used to be here was stale, and is gone
 *
 * This file used to say the surface refuses, that no mutation was declared
 * because "there is nothing to delete", and that the listing is "expected to
 * FAIL with 501 on this platform". None of that has been true since migration
 * 0107 gave the admission plane a store: the listing answers 200 from tables,
 * registration records a revision, and DELETE revokes one. Migration 0109 adds
 * the policy overlay that lets a revision be ACTIVATED, which is what the two
 * mutations below call.
 *
 * The 501 path still exists and is still read: a deployment that has not applied
 * 0107 or 0109 has no admission plane, and the page renders the server's own
 * sentence for it. What changed is that 501 is now the exception rather than the
 * whole story — which is exactly why the prose had to go. A comment describing a
 * refusal that no longer happens is the "disclosed gap" shape this repository
 * has been bitten by: it reads as current, and nothing fails when it stops being
 * true.
 *
 * The reference client (`admin_ui`'s `serviceDescriptorsApi.js`) declares a
 * delete, and this file still does not: DELETE revokes rather than removes, it is
 * terminal, and no page control issues it yet. Deactivate is the recoverable
 * verb and is what the table offers.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as `./adminConfigurationApi.ts`.
 *
 * The wire contract mirrors the Go handler in
 * `services/elitea-main/internal/api/v2/eliteacore/service_descriptors.go`, which
 * keeps the legacy path and mode (`legacy/plugins/elitea_core/api/v2/admin.py`)
 * so the existing admin_ui client reaches the same endpoint unchanged — and now
 * gets the same honest 501 rather than three invented rows.
 *
 * ## What is reused
 *
 * `unwrapBody` (added by the Users port) and `configFailureReason` /
 * `configFailureStatus` from `./adminConfigurationApi`. Those two readers are
 * exactly this page's need — it renders the server's own sentence and has to tell
 * 501 from a load failure — and re-deriving them here would be a second copy to
 * drift. Nothing else: a different endpoint and its own query-key namespace.
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

/**
 * Only `administration` reads this, server-side and in the legacy runtime
 * before it — `api_tools.APIBase` registers `mode_handlers = {'administration':
 * AdminAPI}` and no other mode. It is a static path segment on both sides
 * rather than a parameter, so there is no mode a caller could substitute.
 */
const ADMIN_MODE = 'administration';

const DESCRIPTORS_URL = `/elitea_core/admin/${ADMIN_MODE}`;

/**
 * One row of the legacy `GET /elitea_core/admin/administration`.
 *
 * `healthy` IS THREE-STATE, and the `| null` is the whole point of it.
 *
 * The legacy runtime had two in-process dicts and a descriptor landed in one of
 * them at plugin load, so "nobody has probed this" and "this is down" were the
 * same value — a provider nobody had asked about read as unhealthy. The Go
 * admission plane stores health as a separate PROJECTION with a timestamp
 * (`provider_health_projection`) and answers `null` when no projection is
 * younger than its freshness window.
 *
 * Typing this `boolean` would force the server to pick one of the two lies it
 * was built to stop telling, so the type is what holds the contract: see
 * `services/elitea-main/internal/api/v2/eliteacore/provider_admission.go`.
 */
export interface AdminServiceDescriptor {
  readonly project_id: number;
  readonly provider_name: string;
  readonly service_location_url: string;
  readonly healthy: boolean | null;
  /**
   * The latest admission's state: `active`, `inactive`, `revoked`, or
   * `unregistered` when no revision has ever been recorded.
   *
   * IT IS NOT OPTIONAL DECORATION. The listing is driven by
   * `provider_origin_registration` — every origin ever registered — so a
   * REVOKED provider stays in it. That is deliberate: an admission that was
   * once in force is a fact about what this deployment ran, and DELETE never
   * removes the row. But it means a listing that does not render this column
   * shows a revoked provider identically to a live one, and an operator who
   * revokes one, reloads, and sees no change concludes the revoke did not work.
   */
  readonly status?: string;
  /** Why the latest admission is in that state; empty when it says nothing. */
  readonly reason?: string;
  readonly published_manifest_digest?: string | null;
}

/**
 * How this deployment treats a provider that is recorded but not in force —
 * `ELITEA_PROVIDER_ADMISSION`, read server-side.
 *
 * IT TRAVELS WITH THE ROWS BECAUSE `inactive` MEANS TWO DIFFERENT THINGS. Under
 * `record` an inactive provider still serves every invoke; under `enforce` it is
 * refused. A listing that showed the status without the posture would read
 * identically in the two deployments where the same row has opposite
 * consequences, and the operator has no other way to see which one they are on.
 */
type AdmissionPosture = 'record' | 'enforce';

/** The legacy listing body. `rows` is what the reference client reads. */
interface AdminServiceDescriptorList {
  readonly rows?: readonly AdminServiceDescriptor[];
  readonly total?: number;
  readonly admission_posture?: string;
}

/**
 * What the page renders: the rows AND the posture they have to be read under.
 *
 * A bare array would have been less churn and would have dropped the posture on
 * the floor. `posture` is `undefined` when the server did not send one — an
 * older build, or a body this client does not fully know — and the page says
 * nothing rather than guessing `record`, because guessing would put a
 * reassuring word on the screen for a deployment that might be enforcing.
 */
export interface AdminServiceDescriptorListing {
  readonly rows: readonly AdminServiceDescriptor[];
  readonly posture?: AdmissionPosture | undefined;
}

function readPosture(value: unknown): AdmissionPosture | undefined {
  return value === 'record' || value === 'enforce' ? value : undefined;
}

/**
 * One query-key namespace, declared once — the read/write key-namespace split
 * that made saved data look absent in #132.
 *
 * Not exported. There is no write to invalidate it and no other module that
 * reads this listing, so an export would be a symbol with no importer — the
 * dead-code shape this unit has removed six times (see #126/#129/#134/#136/
 * #138/#149). `ServiceDescriptors.test.tsx` pins the namespace by asserting the
 * literal key against the query cache, which is a stronger pin than importing
 * the builder: a literal does not move when the builder does.
 */
const adminServiceDescriptorKeys = {
  list: () => ['admin', 'service-descriptors', 'list'] as const,
};

/**
 * `GET /elitea_core/admin/administration`.
 *
 * Answers 200 from storage where migration 0107 has been applied, and 501 —
 * with the server's own sentence, which the page renders — where it has not.
 * `retry: false` because a declared-unavailable surface is not a transient
 * error and retrying it three times only delays the explanation.
 */
export function useAdminServiceDescriptors(): UseQueryResult<
  AdminServiceDescriptorListing,
  Error
> {
  return useQuery({
    queryKey: adminServiceDescriptorKeys.list(),
    retry: false,
    queryFn: async (): Promise<AdminServiceDescriptorListing> => {
      // `eliteaFetch` resolves the transport envelope, not the body. Reading
      // `rows` off the envelope is #132's silent empty state — and on this page
      // it would be worse than empty: an unwrapped envelope has no `rows`, so
      // the page would render "no descriptors" instead of the reason, which is
      // precisely the "looks fine, is wrong" outcome this port exists to remove.
      const body = unwrapBody(await eliteaFetch<unknown>(DESCRIPTORS_URL)) as
        | AdminServiceDescriptorList
        | undefined;
      return { rows: body?.rows ?? [], posture: readPosture(body?.admission_posture) };
    },
  });
}

/* ── activation (migration 0109) ────────────────────────────────────────── */

/**
 * `POST /elitea_core/register_descriptor/{project_id}/activate`.
 *
 * `expected_digest` IS NOT OPTIONAL, and it is not a formality. The operator
 * reviewed a manifest and wrote a policy about it; if the provider republished
 * in between, the revision now cites bytes nobody looked at and the server
 * answers 422 rather than putting a reviewed policy in force over an unreviewed
 * manifest. The page sends the digest from the ROW IT IS SHOWING, so the value
 * asserts what the operator actually saw — sending a digest re-read at click
 * time would assert nothing at all.
 */
export interface ActivateServiceDescriptor {
  readonly projectId: number;
  readonly providerName: string;
  readonly expectedDigest: string;
  readonly reason: string;
  /** Reviewed facts. v1 pins no schema; an empty object is a valid overlay. */
  readonly overlay?: Readonly<Record<string, unknown>>;
}

export interface DeactivateServiceDescriptor {
  readonly projectId: number;
  readonly providerName: string;
  readonly reason: string;
}

function registerDescriptorUrl(projectId: number, verb: string, providerName: string): string {
  return (
    `/elitea_core/register_descriptor/${projectId}/${verb}` +
    `?provider_name=${encodeURIComponent(providerName)}`
  );
}

export function useActivateServiceDescriptor(): UseMutationResult<
  void,
  Error,
  ActivateServiceDescriptor
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: ActivateServiceDescriptor) => {
      await eliteaFetch<unknown>(
        registerDescriptorUrl(input.projectId, 'activate', input.providerName),
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            expected_digest: input.expectedDigest,
            reason: input.reason,
            overlay: input.overlay ?? {},
          }),
        },
      );
    },
    // The SAME namespace the listing declares. A key built somewhere else is a
    // cache nothing can reach — #132's read/write split, which made saved data
    // look absent.
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: adminServiceDescriptorKeys.list() }),
  });
}

/**
 * `POST …/deactivate`. NOT a DELETE: DELETE on this surface revokes, which is
 * terminal and records who and when. Deactivating returns a revision to the
 * state registration left it in, so it can be put back in force without the
 * provider republishing.
 */
export function useDeactivateServiceDescriptor(): UseMutationResult<
  void,
  Error,
  DeactivateServiceDescriptor
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: DeactivateServiceDescriptor) => {
      await eliteaFetch<unknown>(
        `${registerDescriptorUrl(input.projectId, 'deactivate', input.providerName)}` +
          `&reason=${encodeURIComponent(input.reason)}`,
        { method: 'POST' },
      );
    },
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: adminServiceDescriptorKeys.list() }),
  });
}

/**
 * The server's own explanation of a refusal, and its status.
 *
 * Re-exported rather than re-derived: `EliteaApiError`'s failure shape is one
 * thing, and two readers of it would be two things to keep in step.
 */
export { configFailureReason as descriptorFailureReason, configFailureStatus as descriptorFailureStatus };
