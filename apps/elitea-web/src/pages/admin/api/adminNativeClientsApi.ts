/**
 * Data layer for the native client registry and the admin device list
 * (ADR-0025 WP2/WP3).
 *
 * Wire contract: `services/elitea-main/internal/api/nativeauth/admin.go` and
 * `devices.go`, described in `v2.yaml` (operations `listNativeClients`,
 * `saveNativeClient`, `deleteNativeClient`, `listNativeDevicesAdministration`,
 * `revokeNativeDeviceAdministration`). Every call goes through the GENERATED
 * fetchers; this module only adds what orval cannot: write hooks
 * (`orval.config.ts`'s `useQuery: true` makes every generated `useXxx` a query,
 * whatever the verb) and one place that peels the transport envelope.
 *
 *   GET    /admin/native_clients/administration
 *   PUT    /admin/native_clients/administration/{client_id}
 *   DELETE /admin/native_clients/administration/{client_id}
 *   GET    /admin/native_devices/administration?user_id=&state=&limit=
 *   DELETE /admin/native_devices/administration/{device_id}
 *
 * ## Two permissions, as the server enforces them
 *
 * The registry routes require `configuration.native_clients`; the device
 * routes require `admin.auth.users` (whoever may suspend an account may cut
 * off its devices). Both route sets are mounted only when the native
 * authorization server is composed, so a 404 here means "this deployment
 * does not serve native sign-in", not "empty".
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import {
  deleteNativeClient,
  getListNativeClientsQueryKey,
  listNativeClients,
  listNativeDevicesAdministration,
  revokeNativeDeviceAdministration,
  saveNativeClient,
} from '@/shared/api/generated/admin/admin';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type {
  NativeAdminDevice,
  NativeClient,
  NativeClientSaveRequest,
} from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

/**
 * The `managed_surface` the server declares on the `native_clients`
 * Configuration section (`config_schemas.go`'s `nativeClientsSection`).
 * Exported so the page's registry keys on the SERVER's word.
 */
export const NATIVE_CLIENTS_MANAGED_SURFACE = 'native_clients';

/** The registry layer a client comes from (`internal/nativeauth/registry.go`). */
export const NATIVE_CLIENT_SOURCE_FILE = 'file';

/** The admin device list's page cap (`devices.go`: limit is capped at 200). */
const ADMIN_DEVICE_LIMIT = 200;

/**
 * The query-key roots, declared once. Every write invalidates both: a client
 * save that disables it revokes devices, and a device revoke changes the
 * client's live-device count.
 */
const nativeAdminKeys = {
  clients: getListNativeClientsQueryKey,
  devicesRoot: ['admin', 'nativeDevices'] as const,
  devices: (userId: number) => ['admin', 'nativeDevices', 'user', userId] as const,
};

/** `GET /admin/native_clients/administration`. */
export function useAdminNativeClients(): UseQueryResult<readonly NativeClient[], Error> {
  return useQuery({
    queryKey: nativeAdminKeys.clients(),
    queryFn: async ({ signal }): Promise<readonly NativeClient[]> => {
      // `eliteaFetch` resolves the transport envelope, not the body (#132).
      const body = unwrapBody(await listNativeClients({ signal })) as
        | { rows?: NativeClient[] }
        | undefined;
      return body?.rows ?? [];
    },
  });
}

/** What the editor dialog collects for one client. */
export interface NativeClientDraft {
  readonly clientId: string;
  readonly displayName: string;
  readonly redirectUris: readonly string[];
  readonly enabled: boolean;
  readonly minClientVersion: string;
}

/** The server's answer to a save or a delete. */
export interface NativeClientWriteOutcome {
  readonly revokedDevices: number;
}

function writeOutcome(response: unknown): NativeClientWriteOutcome {
  const body = unwrapBody(response) as { revoked_devices?: number } | undefined;
  return { revokedDevices: body?.revoked_devices ?? 0 };
}

/** Every write refreshes the registry AND the device lists. */
function useInvalidateNativeAdmin(): () => Promise<void> {
  const queryClient = useQueryClient();
  return async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: nativeAdminKeys.clients() }),
      queryClient.invalidateQueries({ queryKey: nativeAdminKeys.devicesRoot }),
    ]);
  };
}

/** `PUT /admin/native_clients/administration/{client_id}`. */
export function useSaveNativeClient(): UseMutationResult<
  NativeClientWriteOutcome,
  Error,
  NativeClientDraft
> {
  const invalidate = useInvalidateNativeAdmin();
  return useMutation({
    mutationFn: async (draft: NativeClientDraft) => {
      // Every field is sent: the PUT is an upsert of the whole row, and an
      // absent `enabled` would read as `true` on the server — re-enabling a
      // client an operator only meant to rename.
      const body: NativeClientSaveRequest = {
        display_name: draft.displayName,
        redirect_uris: [...draft.redirectUris],
        enabled: draft.enabled,
        min_client_version: draft.minClientVersion,
      };
      return writeOutcome(await saveNativeClient(encodeURIComponent(draft.clientId), body));
    },
    onSuccess: invalidate,
  });
}

/** `DELETE /admin/native_clients/administration/{client_id}`. */
export function useDeleteNativeClient(): UseMutationResult<NativeClientWriteOutcome, Error, string> {
  const invalidate = useInvalidateNativeAdmin();
  return useMutation({
    mutationFn: async (clientId: string) =>
      writeOutcome(await deleteNativeClient(encodeURIComponent(clientId))),
    onSuccess: invalidate,
  });
}

/** One account's devices as the drawer shows them. */
export interface AdminUserNativeDevices {
  /** Every live device first, then the most recent revoked history. */
  readonly devices: readonly NativeAdminDevice[];
  /** How many revoked devices the account has in all. */
  readonly revokedTotal: number;
  /** How many of them `devices` holds (fewer than `revokedTotal` when cut). */
  readonly revokedShown: number;
}

/** A guard against a server whose `total` never stops growing. */
const MAX_ACTIVE_PAGES = 50;

/**
 * `GET /admin/native_devices/administration?user_id=…` — twice.
 *
 * The drawer's status column is the point: an operator looking at a
 * compromised account needs the device that was already revoked, and why, not
 * only the ones still live. But it must never MISS a live one. The server
 * orders by `last_seen_at DESC` and caps a page at 200, and revoked rows are
 * never purged, so one page of `state=all` let 200 newer revoked sessions push
 * an idle-but-live device out of the drawer with nothing saying so (PR #1051
 * review F1). So: every page of `state=active` (offset paging up to `total`),
 * then the first page of `state=revoked`, with its `total` so the drawer can
 * say the history was cut.
 */
export function useAdminUserNativeDevices(
  userId: number | undefined,
): UseQueryResult<AdminUserNativeDevices, Error> {
  return useQuery({
    queryKey: nativeAdminKeys.devices(userId ?? 0),
    enabled: userId !== undefined,
    queryFn: async ({ signal }): Promise<AdminUserNativeDevices> => {
      const page = async (state: 'active' | 'revoked', offset: number) =>
        (unwrapBody(
          await listNativeDevicesAdministration(
            { user_id: userId, state, limit: ADMIN_DEVICE_LIMIT, offset },
            { signal },
          ),
        ) as { rows?: NativeAdminDevice[]; total?: number } | undefined) ?? {};

      const active: NativeAdminDevice[] = [];
      for (let pageIndex = 0; pageIndex < MAX_ACTIVE_PAGES; pageIndex += 1) {
        const body = await page('active', active.length);
        const rows = body.rows ?? [];
        active.push(...rows);
        if (rows.length === 0 || active.length >= (body.total ?? 0)) break;
      }

      const revokedBody = await page('revoked', 0);
      const revoked = revokedBody.rows ?? [];
      return {
        devices: [...active, ...revoked],
        revokedTotal: Math.max(revokedBody.total ?? 0, revoked.length),
        revokedShown: revoked.length,
      };
    },
  });
}

/** `DELETE /admin/native_devices/administration/{device_id}`. */
export function useRevokeAdminNativeDevice(): UseMutationResult<void, Error, string> {
  const invalidate = useInvalidateNativeAdmin();
  return useMutation({
    mutationFn: async (deviceId: string) => {
      await revokeNativeDeviceAdministration(encodeURIComponent(deviceId));
    },
    onSuccess: invalidate,
  });
}

/** What a refused write said, read from the server's body. */
export interface NativeClientFailure {
  readonly status: number | undefined;
  /** `message` — the server's sentence, when it sent one (409). */
  readonly message: string | undefined;
  /** `reasons` — one per field: `client_id`, `display_name`, `redirect_uris[N]`, … (422). */
  readonly reasons: Readonly<Record<string, string>>;
}

function stringRecord(value: unknown): Record<string, string> {
  if (typeof value !== 'object' || value === null) return {};
  const out: Record<string, string> = {};
  for (const [key, reason] of Object.entries(value)) {
    if (typeof reason === 'string') out[key] = reason;
  }
  return out;
}

/** Reads a failed write. Anything that is not an HTTP answer has no body to read. */
export function nativeClientFailure(error: unknown): NativeClientFailure {
  if (!(error instanceof EliteaApiError)) {
    return { status: undefined, message: undefined, reasons: {} };
  }
  const failure = error.failure;
  if (failure.kind === 'auth') {
    return { status: failure.status, message: undefined, reasons: {} };
  }
  if (failure.kind !== 'http') {
    return { status: undefined, message: undefined, reasons: {} };
  }
  const body = (typeof failure.body === 'object' && failure.body !== null ? failure.body : {}) as {
    message?: unknown;
    reasons?: unknown;
  };
  return {
    status: failure.status,
    message: typeof body.message === 'string' && body.message !== '' ? body.message : undefined,
    reasons: stringRecord(body.reasons),
  };
}

/** True when the route is not mounted: this deployment serves no native sign-in. */
export function isNativeServerAbsent(error: unknown): boolean {
  return nativeClientFailure(error).status === 404;
}
