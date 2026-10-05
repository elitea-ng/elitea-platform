/**
 * State and handlers for Admin › Branding (ADR-0024 WP4).
 *
 * Kept out of the page component for the reason every other admin page does
 * it: the page file is layout and identity, the behaviour is testable without
 * rendering, and the two do not grow into one file.
 *
 * ## The draft is an overlay, and the payload is the whole section
 *
 * The server answers `values` for every declared key. The page keeps only the
 * keys the operator touched in `draft`, lays them over the server's values for
 * rendering, and on save sends the MERGED record of the keys it edits. The PUT
 * is a per-key UPSERT, not a replace: a key the payload omits keeps its stored
 * value. A save relies on that to leave the keys this page does not edit
 * (`scheme_tokens`, `sender_name`, `support_email`) alone; a reset must
 * therefore name them explicitly — see `resetBrandingPayload`.
 *
 * ## The preview pack
 *
 * `previewPack` is what the bootstrap route would serve after a save: the
 * effective pack (or the compiled default) with the draft applied field for
 * field, the way the Go resolver applies its database overlay. See
 * `brandingValues.ts` for the one place the two deliberately differ.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import type { BrandPack } from '@/shared/brand';
import { t } from '@/shared/i18n';

import {
  brandingFailureReason,
  useBrandingSettings,
  useSaveBrandingSettings,
  useUploadBrandingAsset,
  type BrandingAssetKind,
} from './api/adminBrandingApi';
import { useBrandingDirtyStore } from './brandingDirty.store';
import {
  applyDraftToPack,
  basePackFrom,
  brandingErrorField,
  effectiveFontFaces,
  effectiveLogoEmail,
  emptyBrandingValues,
  resetBrandingPayload,
  parseBrandingValues,
  withDerivedSchemes,
  type BrandingAssetKey,
  type BrandingFontFace,
  type BrandingKey,
  type BrandingLayers,
  type BrandingNumberKey,
  type BrandingTextKey,
  type BrandingValues,
} from './brandingValues';

export interface BrandingToast {
  readonly severity: 'success' | 'error';
  readonly message: string;
}

export interface BrandingFieldError {
  readonly key: BrandingKey | undefined;
  readonly message: string;
}

/** Where an uploaded asset's path is written. */
export type BrandingUploadTarget =
  | { readonly field: BrandingAssetKey }
  | { readonly faceIndex: number };

export interface BrandingUploadError {
  readonly kind: BrandingAssetKind;
  readonly message: string;
}

export type BrandingScalarKey = BrandingTextKey | BrandingNumberKey;

export interface AdminBrandingPageState {
  readonly isLoading: boolean;
  readonly isLoaded: boolean;
  readonly loadError: string | undefined;
  readonly values: BrandingValues;
  readonly layers: BrandingLayers;
  readonly basePack: BrandPack;
  readonly effectiveFaces: readonly BrandingFontFace[];
  /** The served pack's e-mail logo path, or empty (WP7's `assets.logoEmail`). */
  readonly effectiveLogoEmail: string;
  readonly previewPack: BrandPack;
  readonly isDirty: boolean;
  readonly isSaving: boolean;
  readonly fieldError: BrandingFieldError | undefined;
  readonly onFieldChange: <K extends BrandingScalarKey>(key: K, value: BrandingValues[K]) => void;
  readonly onFontFacesChange: (faces: readonly BrandingFontFace[]) => void;
  readonly onSave: () => void;
  readonly onDiscard: () => void;
  readonly toast: BrandingToast | undefined;
  readonly onDismissToast: () => void;
  /** Shows a toast from outside the hook — the package controls (WP9) report through the page's one Snackbar. */
  readonly onNotify: (toast: BrandingToast) => void;
  readonly resetOpen: boolean;
  readonly onRequestReset: () => void;
  readonly onCancelReset: () => void;
  readonly onConfirmReset: () => void;
  readonly uploadingKind: BrandingAssetKind | undefined;
  readonly uploadError: BrandingUploadError | undefined;
  readonly onUploadAsset: (kind: BrandingAssetKind, file: File, target: BrandingUploadTarget) => void;
}

const NO_LAYERS: BrandingLayers = { file: false, database: false };

/** A face the operator added and never filled in is not sent. */
function withoutBlankFaces(faces: readonly BrandingFontFace[]): BrandingFontFace[] {
  return faces.filter((face) => face.family.trim() !== '' || face.url.trim() !== '');
}

function writeUploadedPath(
  faces: readonly BrandingFontFace[],
  index: number,
  url: string,
): BrandingFontFace[] {
  const next = faces.map((face) => ({ ...face }));
  const existing = next[index];
  if (existing === undefined) next[index] = { family: '', url };
  else next[index] = { ...existing, url };
  return next;
}

export function useAdminBrandingPage(): AdminBrandingPageState {
  const query = useBrandingSettings();
  const save = useSaveBrandingSettings();
  const upload = useUploadBrandingAsset();

  const [draft, setDraft] = useState<Partial<BrandingValues>>({});
  const [toast, setToast] = useState<BrandingToast | undefined>(undefined);
  const [resetOpen, setResetOpen] = useState(false);
  const [uploadingKind, setUploadingKind] = useState<BrandingAssetKind | undefined>(undefined);
  const [uploadError, setUploadError] = useState<BrandingUploadError | undefined>(undefined);

  const serverValues = useMemo(
    () => (query.data === undefined ? emptyBrandingValues() : parseBrandingValues(query.data.values)),
    [query.data],
  );
  const values = useMemo((): BrandingValues => ({ ...serverValues, ...draft }), [serverValues, draft]);
  const layers = query.data?.layers ?? NO_LAYERS;
  const basePack = useMemo(() => basePackFrom(query.data?.effective), [query.data]);
  const effectiveFaces = useMemo(() => effectiveFontFaces(query.data?.effective), [query.data]);
  const logoEmail = useMemo(() => effectiveLogoEmail(query.data?.effective), [query.data]);
  const previewPack = useMemo(
    () => withDerivedSchemes(applyDraftToPack(basePack, values), basePack.brand.hue),
    [basePack, values],
  );

  const isDirty = useMemo(
    () => JSON.stringify(values) !== JSON.stringify(serverValues),
    [values, serverValues],
  );

  // The guard reads this flag live (`brandingDirty.store.ts`); the effect keeps
  // it in step with the draft and lowers it when the page unmounts.
  useEffect(() => {
    useBrandingDirtyStore.getState().setDirty(isDirty);
    return () => {
      useBrandingDirtyStore.getState().setDirty(false);
    };
  }, [isDirty]);

  const fieldError = useMemo((): BrandingFieldError | undefined => {
    const reason = brandingFailureReason(save.error);
    if (reason === undefined) {
      return save.error === null ? undefined : { key: undefined, message: save.error.message };
    }
    return { key: brandingErrorField(reason), message: reason };
  }, [save.error]);

  const onFieldChange = useCallback(
    <K extends BrandingScalarKey>(key: K, value: BrandingValues[K]) => {
      save.reset();
      setDraft((previous) => ({ ...previous, [key]: value }));
    },
    [save],
  );

  const onFontFacesChange = useCallback(
    (faces: readonly BrandingFontFace[]) => {
      save.reset();
      setDraft((previous) => ({ ...previous, font_faces: faces }));
    },
    [save],
  );

  const onDiscard = useCallback(() => {
    save.reset();
    setDraft({});
    setUploadError(undefined);
  }, [save]);

  const onSave = useCallback(() => {
    const payload: BrandingValues = { ...values, font_faces: withoutBlankFaces(values.font_faces) };
    save.mutate(payload, {
      onSuccess: () => {
        setDraft({});
        setToast({
          severity: 'success',
          message: t(
            'pages.admin.branding.toast.saved',
            'Branding saved. Users see it on their next page load.',
          ),
        });
      },
      onError: (error) => {
        setToast({
          severity: 'error',
          message:
            brandingFailureReason(error) ??
            t('pages.admin.branding.toast.saveFailed', 'The branding could not be saved.'),
        });
      },
    });
  }, [save, values]);

  const onRequestReset = useCallback(() => setResetOpen(true), []);
  const onCancelReset = useCallback(() => setResetOpen(false), []);
  const onConfirmReset = useCallback(() => {
    setResetOpen(false);
    save.mutate(resetBrandingPayload(query.data?.values), {
      onSuccess: () => {
        setDraft({});
        setToast({
          severity: 'success',
          message: t(
            'pages.admin.branding.toast.reset',
            'Branding reset. Every field now inherits from the layer below.',
          ),
        });
      },
      onError: (error) => {
        setToast({
          severity: 'error',
          message:
            brandingFailureReason(error) ??
            t('pages.admin.branding.toast.resetFailed', 'The branding could not be reset.'),
        });
      },
    });
  }, [save, query.data]);

  const onUploadAsset = useCallback(
    (kind: BrandingAssetKind, file: File, target: BrandingUploadTarget) => {
      setUploadError(undefined);
      setUploadingKind(kind);
      upload.mutate(
        { kind, file },
        {
          onSuccess: (asset) => {
            save.reset();
            setDraft((previous) => {
              if ('field' in target) return { ...previous, [target.field]: asset.path };
              const faces = previous.font_faces ?? serverValues.font_faces;
              return { ...previous, font_faces: writeUploadedPath(faces, target.faceIndex, asset.path) };
            });
          },
          onError: (error) => {
            setUploadError({
              kind,
              message:
                brandingFailureReason(error) ??
                t('pages.admin.branding.upload.failed', 'The file could not be uploaded.'),
            });
          },
          onSettled: () => setUploadingKind(undefined),
        },
      );
    },
    [save, serverValues.font_faces, upload],
  );

  return {
    isLoading: query.isPending,
    isLoaded: query.isSuccess,
    loadError:
      query.error === null || query.error === undefined
        ? undefined
        : (brandingFailureReason(query.error) ?? query.error.message),
    values,
    layers,
    basePack,
    effectiveFaces,
    effectiveLogoEmail: logoEmail,
    previewPack,
    isDirty,
    isSaving: save.isPending,
    fieldError,
    onFieldChange,
    onFontFacesChange,
    onSave,
    onDiscard,
    toast,
    onDismissToast: useCallback(() => setToast(undefined), []),
    onNotify: setToast,
    resetOpen,
    onRequestReset,
    onCancelReset,
    onConfirmReset,
    uploadingKind,
    uploadError,
    onUploadAsset,
  };
}
