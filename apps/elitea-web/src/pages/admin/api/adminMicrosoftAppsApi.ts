/**
 * Data layer for the Microsoft Default app registry (ADR-0057 decisions 5 and 11;
 * epic 2254 story S1b).
 *
 * Wire contract: `services/elitea-main/internal/api/v2/admin/microsoft_oauth_apps.go`,
 * described in `v2.yaml` (operations `listMicrosoftApps`, `createMicrosoftApp`,
 * `updateMicrosoftApp`, `disableMicrosoftApp`, `enableMicrosoftApp`,
 * `testMicrosoftApp`, `saveMicrosoftAppSettings`). Every call goes through the
 * GENERATED fetchers; this module adds the write hooks (`orval.config.ts`'s
 * `useQuery: true` makes every generated `useXxx` a query) and one place that peels
 * the transport envelope.
 *
 * ## The client secret is write-only
 *
 * No read carries it: a listing says `secret_set` and the expiry. The write body
 * carries it only when the administrator typed one, and a draft never keeps it:
 * it goes out in one request and the form forgets it.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from '@tanstack/react-query';

import {
  createMicrosoftApp,
  disableMicrosoftApp,
  enableMicrosoftApp,
  getListMicrosoftAppsQueryKey,
  listMicrosoftApps,
  saveMicrosoftAppSettings,
  testMicrosoftApp,
  updateMicrosoftApp,
} from '@/shared/api/generated/admin/admin';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type {
  MicrosoftApp,
  MicrosoftAppList,
  MicrosoftAppRequest,
  MicrosoftAppTestResult,
} from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

/** The `managed_surface` the server declares on the `microsoft_apps` Configuration section. */
export const MICROSOFT_APPS_MANAGED_SURFACE = 'microsoft_apps';

/** `GET /admin/microsoft_oauth_apps`: the apps, the settings and the Entra checklist. */
export function useAdminMicrosoftApps(): UseQueryResult<MicrosoftAppList, Error> {
  return useQuery({
    queryKey: getListMicrosoftAppsQueryKey(),
    retry: false,
    queryFn: async ({ signal }) => unwrapBody(await listMicrosoftApps({ signal })) as MicrosoftAppList,
  });
}

function useInvalidateMicrosoftApps(): () => Promise<void> {
  const queryClient = useQueryClient();
  return () => queryClient.invalidateQueries({ queryKey: getListMicrosoftAppsQueryKey() });
}

/** What the dialog saves: an id when editing, nothing when creating. */
export interface MicrosoftAppSave {
  readonly id: string | undefined;
  readonly body: MicrosoftAppRequest;
}

/** `POST` or `PUT /admin/microsoft_oauth_apps[/{id}]`. */
export function useSaveMicrosoftApp(): UseMutationResult<MicrosoftApp, Error, MicrosoftAppSave> {
  const invalidate = useInvalidateMicrosoftApps();
  return useMutation({
    mutationFn: async ({ id, body }: MicrosoftAppSave) =>
      unwrapBody(
        id === undefined ? await createMicrosoftApp(body) : await updateMicrosoftApp(encodeURIComponent(id), body),
      ) as MicrosoftApp,
    onSuccess: invalidate,
  });
}

/** What the row switch asks for. */
export interface MicrosoftAppEnabledChange {
  readonly id: string;
  readonly enabled: boolean;
}

/** `POST /admin/microsoft_oauth_apps/{id}/enable` or `/disable`. */
export function useSetMicrosoftAppEnabled(): UseMutationResult<MicrosoftApp, Error, MicrosoftAppEnabledChange> {
  const invalidate = useInvalidateMicrosoftApps();
  return useMutation({
    mutationFn: async ({ id, enabled }: MicrosoftAppEnabledChange) =>
      unwrapBody(
        enabled ? await enableMicrosoftApp(encodeURIComponent(id)) : await disableMicrosoftApp(encodeURIComponent(id)),
      ) as MicrosoftApp,
    onSuccess: invalidate,
  });
}

/** `POST /admin/microsoft_oauth_apps/{id}/test`. A read of Entra's metadata, no write. */
export function useTestMicrosoftApp(): UseMutationResult<MicrosoftAppTestResult, Error, string> {
  return useMutation({
    mutationFn: async (id: string) => unwrapBody(await testMicrosoftApp(encodeURIComponent(id))) as MicrosoftAppTestResult,
  });
}

/** `PUT /admin/microsoft_oauth_apps/settings`: allow or refuse Custom app mode. */
export function useSaveMicrosoftAppSettings(): UseMutationResult<unknown, Error, boolean> {
  const invalidate = useInvalidateMicrosoftApps();
  return useMutation({
    mutationFn: async (customApps: boolean) => unwrapBody(await saveMicrosoftAppSettings({ custom_apps: customApps })),
    onSuccess: invalidate,
  });
}

/** What a refused write said. `field` names the form field the server refused. */
export interface MicrosoftAppFailure {
  readonly status: number | undefined;
  readonly message: string | undefined;
  readonly field: string | undefined;
}

/** Reads a failed write. Anything that is not an HTTP answer has no body to read. */
export function microsoftAppFailure(error: unknown): MicrosoftAppFailure {
  const none: MicrosoftAppFailure = { status: undefined, message: undefined, field: undefined };
  if (!(error instanceof EliteaApiError)) return none;
  const failure = error.failure;
  if (failure.kind === 'auth') return { ...none, status: failure.status };
  if (failure.kind !== 'http') return none;
  const body = (typeof failure.body === 'object' && failure.body !== null ? failure.body : {}) as {
    error?: unknown;
    field?: unknown;
  };
  return {
    status: failure.status,
    message: typeof body.error === 'string' && body.error !== '' ? body.error : undefined,
    field: typeof body.field === 'string' && body.field !== '' ? body.field : undefined,
  };
}
