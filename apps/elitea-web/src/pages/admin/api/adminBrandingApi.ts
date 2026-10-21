/**
 * REST client for the admin Branding page (ADR-0024 WP4) —
 * `/api/v2/admin/branding/administration` and `/api/v2/admin/branding/assets/{kind}`.
 *
 * Generated, unlike the other files in this directory: `v2.yaml` describes
 * these four operations, so the fetchers come from `shared/api/generated/
 * admin/admin.ts`. What this module adds is the shape orval cannot: the
 * read is a `useQuery` over the generated fetcher, and the three writes are
 * locally-owned `useMutation`s over the generated `saveBrandingSettings` /
 * `resetBrandingSettings` / `uploadBrandingAsset` — NOT the generated
 * `useSaveBrandingSettings` / `useResetBrandingSettings` /
 * `useUploadBrandingAsset`, which orval emits query-shaped for a PUT, a
 * DELETE and a POST (the same generated-shape mismatch `features/apps/api/
 * useModerationRequests.ts` documents). A query-shaped write would dedupe an
 * unchanged resubmission against its cache and auto-retry a transient
 * failure; `useMutation` does neither.
 *
 * Every route is gated server-side on `configuration.branding`
 * (`internal/api/router.go`). `window.admin_ui_config.permissions` hides the
 * nav item and gates nothing — see `../adminUiConfig`.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import {
  getBrandingSettings,
  getGetBrandingSettingsQueryKey,
  resetBrandingSettings,
  saveBrandingSettings,
  uploadBrandingAsset,
} from '@/shared/api/generated/admin/admin';
import type { BrandingAsset, BrandingSettings } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';

/** The upload kinds the asset route accepts, as the generated client spells them. */
export type BrandingAssetKind = Parameters<typeof uploadBrandingAsset>[0];

/**
 * The server's own reason for a refusal, when it gave one. The branding
 * routes state each 400 with the key it applies to (`"brand_hue" must be a
 * six-digit hex colour …`), and an upload refusal names what was wrong with
 * the bytes; both are meant to be shown verbatim.
 */
export function brandingFailureReason(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  if (failure.kind !== 'http') return undefined;
  const body = failure.body;
  if (typeof body !== 'object' || body === null) return undefined;
  const record = body as { error?: unknown; message?: unknown };
  const reason = typeof record.error === 'string' ? record.error : record.message;
  return typeof reason === 'string' && reason !== '' ? reason : undefined;
}

/** `GET /admin/branding/administration`. */
export function useBrandingSettings(): UseQueryResult<BrandingSettings, Error> {
  return useQuery({
    queryKey: getGetBrandingSettingsQueryKey(),
    queryFn: async ({ signal }): Promise<BrandingSettings> => {
      const response = await getBrandingSettings({ signal });
      // `eliteaFetch` throws on every non-2xx (mutator.ts), so the error arms
      // of the generated union are unreachable here; the status check narrows
      // the type rather than handling a case.
      if (response.status !== 200) throw new Error(`branding read answered ${response.status}`);
      return response.data;
    },
  });
}

/** `PUT /admin/branding/administration` with the FULL `values` object. */
export function useSaveBrandingSettings(): UseMutationResult<
  BrandingSettings,
  Error,
  Record<string, unknown>
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (values: Record<string, unknown>): Promise<BrandingSettings> => {
      const response = await saveBrandingSettings({ values });
      if (response.status !== 200) throw new Error(`branding save answered ${response.status}`);
      return response.data;
    },
    // The save answers the re-read state (`saved: true`), so it IS the fresh
    // read; setting it avoids a second round trip and a flash of the old
    // values, and the invalidation keeps any other observer honest.
    onSuccess: (settings) => {
      queryClient.setQueryData(getGetBrandingSettingsQueryKey(), settings);
      return queryClient.invalidateQueries({ queryKey: getGetBrandingSettingsQueryKey() });
    },
  });
}

/**
 * `DELETE /admin/branding/administration`: the server deletes every row of
 * the section, so every key inherits again — the keys this page never
 * renders (a package import's `scheme_tokens`, the sender fields, any key
 * added later) included (issue 1057). It answers the re-read state, as the
 * save does.
 */
export function useResetBrandingSettings(): UseMutationResult<BrandingSettings, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (): Promise<BrandingSettings> => {
      const response = await resetBrandingSettings();
      if (response.status !== 200) throw new Error(`branding reset answered ${response.status}`);
      return response.data;
    },
    onSuccess: (settings) => {
      queryClient.setQueryData(getGetBrandingSettingsQueryKey(), settings);
      return queryClient.invalidateQueries({ queryKey: getGetBrandingSettingsQueryKey() });
    },
  });
}

export interface BrandingUpload {
  readonly kind: BrandingAssetKind;
  readonly file: File;
}

/** `POST /admin/branding/assets/{kind}` — one multipart `file` part. */
export function useUploadBrandingAsset(): UseMutationResult<BrandingAsset, Error, BrandingUpload> {
  return useMutation({
    mutationFn: async ({ kind, file }: BrandingUpload): Promise<BrandingAsset> => {
      const response = await uploadBrandingAsset(kind, { file });
      if (response.status !== 200) throw new Error(`asset upload answered ${response.status}`);
      return response.data;
    },
  });
}
