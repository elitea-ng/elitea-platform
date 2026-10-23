/**
 * REST client for project CREATE and DELETE — the front half of the
 * provisioning pipeline (#333).
 *
 * Split from `./adminProjectsApi.ts`, which owns the listing and the three
 * writes that came with the original port of this page. The split is not
 * arbitrary: these two are the only writes on the page that do NOT belong to
 * the `/admin/...` family. Creation and deletion belong to the projects handler
 * (`services/elitea-main/internal/api/v2/projects/create.go`), which drives
 * `internal/application/projectprovisioning` — ordered steps forward, and the
 * same steps compensated in reverse when one fails.
 *
 * That is why both carry STEP LISTS rather than a bare ok. A provisioning
 * failure is a position in a pipeline, not a status code, and the position is
 * the part an operator can act on.
 *
 * The cache key namespace is imported rather than redeclared: a second one here
 * would be a cache these writes never refresh, which is the read/write key split
 * that made saved data look absent in #132.
 */
import { useMutation, useQueryClient, type UseMutationResult } from '@tanstack/react-query';

import { EliteaApiError, eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

import { ADMIN_MODE, adminProjectsKeys } from './adminProjectsApi';

/**
 * One step of the provisioning pipeline, as
 * `internal/application/projectprovisioning.StepStatus` serialises it.
 *
 * `ok` is THREE-STATE and declared that way on purpose. `null` means the step
 * never ran — the reference distinguishes it from `false`, and collapsing the
 * two would report a step that was never reached as one that failed. The
 * compensation path depends on that distinction: when the tenant schema is held
 * back because the project row survived, the operator needs to read "was not
 * started" rather than "did not complete", because the two call for different
 * actions.
 */
export interface ProvisioningStep {
  readonly step: string;
  readonly initialized: boolean;
  readonly ok: boolean | null;
  /** Caller-safe. The server never puts a raw database error here. */
  readonly msg: string;
}

/** `POST /projects/project/administration`'s body, on both branches. */
export interface CreateProjectResult {
  readonly steps: readonly ProvisioningStep[];
  readonly rollback_steps: readonly ProvisioningStep[];
  /** Present only on 201 — the server omits it when nothing survived. */
  readonly id?: number;
}

export interface CreateProjectInput {
  readonly name: string;
  /** Addresses granted the project `admin` role. The role itself is not ours to choose. */
  readonly adminEmails: readonly string[];
}

/**
 * A failed pipeline run, with the two lists KEPT APART.
 *
 * They are not interchangeable and must not be concatenated. `steps` is how far
 * the pipeline got going forward; `rollback` is what the compensation managed
 * to undo. A forward failure is the expected, already-cleaned-up case. A
 * ROLLBACK failure is the one that leaves orphaned infrastructure behind — a
 * tenant schema the server refused to drop because the project row survived,
 * for instance. The server distinguishes them by message text
 * (`safeStepMessage` vs `heldBackStepMessage`); flattening the two arrays
 * throws away the structural half of that distinction and leaves an operator
 * reading two identical-looking bullet lines that call for opposite actions.
 */
export interface ProvisioningFailure {
  readonly steps: readonly ProvisioningStep[];
  readonly rollback: readonly ProvisioningStep[];
}

/** Nothing failed, or the error carried no pipeline detail at all. */
export const NO_PROVISIONING_FAILURE: ProvisioningFailure = { steps: [], rollback: [] };

/**
 * The step lists the server returns WITH a failure.
 *
 * Both create and delete answer their error status with a body describing how
 * far the pipeline got, and `eliteaFetch` preserves it on
 * `EliteaApiError.failure.body` for `kind: 'http'`. Discarding it would reduce
 * "the tenant schema was kept because the project row is still there" to a bare
 * 500 — which is the one failure an operator has to act on differently.
 */
export function provisioningStepsFromError(error: unknown): ProvisioningFailure {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') {
    return NO_PROVISIONING_FAILURE;
  }
  const body = unwrapBody(error.failure.body);
  if (typeof body !== 'object' || body === null) return NO_PROVISIONING_FAILURE;
  const { steps, rollback_steps: rollback } = body as {
    steps?: unknown;
    rollback_steps?: unknown;
  };
  return { steps: readSteps(steps), rollback: readSteps(rollback) };
}

function readSteps(value: unknown): readonly ProvisioningStep[] {
  if (!Array.isArray(value)) return [];
  return value.filter(
    (entry): entry is ProvisioningStep =>
      typeof entry === 'object' && entry !== null && typeof (entry as ProvisioningStep).step === 'string',
  );
}

/**
 * The steps worth showing an operator: the ones that reported a definite
 * failure, and the ones that were deliberately not started.
 *
 * A step with `ok === true` is noise in a failure report, and there are nine of
 * them. `ok === null` is NOT noise — that is the held-back tenant schema.
 */
export function failedProvisioningSteps(failure: ProvisioningFailure): ProvisioningFailure {
  return {
    steps: failure.steps.filter((entry) => entry.ok !== true),
    rollback: failure.rollback.filter((entry) => entry.ok !== true),
  };
}

/**
 * `POST /projects/project/administration` — provision a project.
 *
 * NOT `/admin/...`: creation lives on the projects handler, gated on
 * `projects.projects.project.create`, and answers only on the `administration`
 * mode (any other mode is a 404 there, as it is in the legacy runtime).
 *
 * The owner is the AUTHENTICATED CALLER and is never sent — the server reads it
 * from the session and ignores any body field claiming otherwise. So is the
 * granted role: the handler hardcodes `admin`, because a caller choosing the
 * role it grants would be a privilege decision made in a request body.
 *
 * The limits are all omitted, which is not the same as sending zeroes: every
 * limit field on the wire is a pointer server-side, so an absent field takes
 * `DefaultLimits()` — `-1` for "unlimited" CPU, `5000` for the VCU ceiling.
 * Sending the Go zero value for those would silently cap a new project at
 * nothing.
 */
export function useCreateAdminProject(): UseMutationResult<
  CreateProjectResult,
  Error,
  CreateProjectInput
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ name, adminEmails }): Promise<CreateProjectResult> => {
      const response = await eliteaFetch<unknown>(`/projects/project/${ADMIN_MODE}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, project_admin_email: [...adminEmails] }),
      });
      const body = unwrapBody(response);
      return (body ?? { steps: [], rollback_steps: [] }) as CreateProjectResult;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminProjectsKeys.all }),
  });
}

/**
 * `DELETE /projects/project/administration/{id}` — deprovision a project.
 *
 * IRREVERSIBLE. The server drops `p_<id> CASCADE`, which takes every prompt,
 * agent, conversation, artifact and audit row in the tenant with it. There is
 * no undo and no soft-delete behind this call; the confirmation in
 * `../AdminProjectDeleteDialog.tsx` is the only thing between an operator and
 * that outcome.
 *
 * One project per call. The dialog loops rather than sending a batch, because
 * the server has no batch route and because a partial failure has to be
 * reported per project — "three of five were deleted" is the answer an operator
 * needs, and a single aggregate rejection cannot give it.
 *
 * THIS MUTATION DOES NOT INVALIDATE, and that is deliberate. react-query AWAITS
 * a promise returned from `onSuccess` before settling `mutateAsync`, so an
 * invalidation here would make every iteration of that loop wait for a full
 * listing refetch before issuing the next DELETE — N round trips of dead time
 * on top of N schema drops, with the dialog's "will be destroyed" list visibly
 * shrinking an entry at a time as the operator watches. The caller invalidates
 * ONCE, after the loop; see `../useAdminProjectProvisioning.ts`. Any new caller
 * must do the same.
 */
export function useDeleteAdminProject(): UseMutationResult<void, Error, { projectId: number }> {
  return useMutation({
    mutationFn: async ({ projectId }) => {
      await eliteaFetch<unknown>(`/projects/project/${ADMIN_MODE}/${projectId}`, {
        method: 'DELETE',
      });
    },
  });
}
