/**
 * State and handlers for the branding package controls on Admin › Branding
 * (ADR-0024 WP9): download, import with a dry run, and the kept versions.
 *
 * Kept beside `useAdminBrandingPage.ts` rather than inside it: the page hook
 * owns the DRAFT, this one owns a flow that REPLACES the whole database layer
 * at once, and the only thing the two share is the unsaved-changes rule below
 * and the page's one toast.
 *
 * ## The unsaved-changes rule
 *
 * An applied package sets every branding key, so a draft on the page would be
 * stale the moment it lands — and a Save after it would write the stale draft
 * back over the package. Opening the import dialog or restoring a version
 * while the draft is dirty therefore asks first, and discards the draft only
 * after the operator confirms; the navigation guard's flag follows the draft.
 *
 * ## The dry run is automatic
 *
 * Picking a file runs `dry_run=true` at once and shows the report; "Apply"
 * runs the import for real. A refused package (400 with the report shape) is
 * shown as its problems, not as a toast — the entry names are the point.
 */
import { useCallback, useState } from 'react';

import type { BrandingPackageReport, BrandingPackageVersion } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { brandingFailureReason } from './api/adminBrandingApi';
import {
  useBrandingPackageVersions,
  useDownloadBrandingPackage,
  useImportBrandingPackage,
  useRestoreBrandingPackageVersion,
} from './api/adminBrandingPackageApi';
import { ADMIN_ITEM } from './adminItemIds';
import { useAdminWritable } from './adminWriteAccess';
import { reportSummary } from './brandingPackage';
import type { BrandingToast } from './useAdminBrandingPage';

type BrandingPackagePhase = 'idle' | 'checking' | 'applying';

/** What the confirmation dialog is asking about. */
export type BrandingPackagePendingAction =
  | { readonly kind: 'import' }
  | { readonly kind: 'restore'; readonly version: BrandingPackageVersion };

export interface BrandingPackageDeps {
  readonly isDirty: boolean;
  /** Drops the page's draft; called only after the operator confirmed. */
  readonly discardDraft: () => void;
  readonly notify: (toast: BrandingToast) => void;
}

export interface BrandingPackageState {
  readonly isDownloading: boolean;
  readonly onDownload: () => void;

  readonly importOpen: boolean;
  readonly onRequestImport: () => void;
  readonly onCloseImport: () => void;
  readonly file: File | undefined;
  readonly onPickFile: (file: File) => void;
  readonly phase: BrandingPackagePhase;
  /** The dry-run report (or the refusal's), once a file was checked. */
  readonly report: BrandingPackageReport | undefined;
  /** A transport-level failure of the check (413, 503, network), as the server put it. */
  readonly checkError: string | undefined;
  readonly canApply: boolean;
  /** Why Apply is withheld, when it is. */
  readonly applyBlockedReason: string | undefined;
  readonly onApply: () => void;

  readonly versions: readonly BrandingPackageVersion[];
  readonly versionsLoading: boolean;
  readonly versionsError: string | undefined;
  readonly restoringDigest: string | undefined;
  readonly onRequestRestore: (version: BrandingPackageVersion) => void;

  readonly pending: BrandingPackagePendingAction | undefined;
  readonly onConfirmPending: () => void;
  readonly onCancelPending: () => void;
}

function failureMessage(error: unknown, fallback: string): string {
  return brandingFailureReason(error) ?? fallback;
}

export function useBrandingPackage({ isDirty, discardDraft, notify }: BrandingPackageDeps): BrandingPackageState {
  // The download is a read (a GET), so it works at the read level; the import
  // (its dry-run check included) and the restore are writes (R3.4b, issue 2613).
  const download = useDownloadBrandingPackage();
  const access = useAdminWritable(ADMIN_ITEM.branding);
  const importPackage = access.guard(useImportBrandingPackage());
  const restore = access.guard(useRestoreBrandingPackageVersion());
  const versions = useBrandingPackageVersions();

  const [importOpen, setImportOpen] = useState(false);
  const [file, setFile] = useState<File | undefined>(undefined);
  const [phase, setPhase] = useState<BrandingPackagePhase>('idle');
  const [report, setReport] = useState<BrandingPackageReport | undefined>(undefined);
  const [checkError, setCheckError] = useState<string | undefined>(undefined);
  const [pending, setPending] = useState<BrandingPackagePendingAction | undefined>(undefined);
  const [restoringDigest, setRestoringDigest] = useState<string | undefined>(undefined);

  const onDownload = useCallback(() => {
    download.mutate(undefined, {
      onSuccess: (result) => {
        if (result.ok) return;
        const error = result.error;
        notify({
          severity: 'error',
          message:
            (error.kind === 'http' ? error.reason : undefined) ??
            t('pages.admin.branding.package.download.failed', 'The branding package could not be downloaded.'),
        });
      },
      onError: () =>
        notify({
          severity: 'error',
          message: t('pages.admin.branding.package.download.failed', 'The branding package could not be downloaded.'),
        }),
    });
  }, [download, notify]);

  const openImport = useCallback(() => {
    setFile(undefined);
    setReport(undefined);
    setCheckError(undefined);
    setPhase('idle');
    setImportOpen(true);
  }, []);

  const onRequestImport = useCallback(() => {
    if (isDirty) setPending({ kind: 'import' });
    else openImport();
  }, [isDirty, openImport]);

  const onCloseImport = useCallback(() => {
    if (phase !== 'idle') return;
    setImportOpen(false);
  }, [phase]);

  const onPickFile = useCallback(
    (picked: File) => {
      setFile(picked);
      setReport(undefined);
      setCheckError(undefined);
      setPhase('checking');
      importPackage.mutate(
        { file: picked, dryRun: true },
        {
          onSuccess: setReport,
          onError: (error) =>
            setCheckError(
              failureMessage(error, t('pages.admin.branding.package.check.failed', 'The package could not be checked.')),
            ),
          onSettled: () => setPhase('idle'),
        },
      );
    },
    [importPackage],
  );

  const applyBlockedReason =
    file === undefined
      ? t('pages.admin.branding.package.apply.noFile', 'Choose a package first.')
      : phase === 'checking'
        ? t('pages.admin.branding.package.apply.checking', 'The package is being checked.')
        : checkError !== undefined
          ? checkError
          : report === undefined
            ? t('pages.admin.branding.package.apply.noReport', 'The package has not been checked.')
            : report.problems.length > 0
              ? t('pages.admin.branding.package.apply.problems', 'The package has problems; fix them and pick it again.')
              : undefined;
  const canApply = applyBlockedReason === undefined && phase === 'idle';

  const onApply = useCallback(() => {
    if (file === undefined || !canApply) return;
    setPhase('applying');
    importPackage.mutate(
      { file, dryRun: false },
      {
        onSuccess: (applied) => {
          if (applied.applied) {
            setImportOpen(false);
            notify({
              severity: 'success',
              message: t(
                'pages.admin.branding.package.applied',
                'Branding package applied. Users see it on their next page load.',
              ),
            });
            return;
          }
          setReport(applied);
          notify({
            severity: 'error',
            message:
              reportSummary(applied) ??
              t('pages.admin.branding.package.apply.failed', 'The branding package could not be applied.'),
          });
        },
        onError: (error) =>
          notify({
            severity: 'error',
            message: failureMessage(
              error,
              t('pages.admin.branding.package.apply.failed', 'The branding package could not be applied.'),
            ),
          }),
        onSettled: () => setPhase('idle'),
      },
    );
  }, [canApply, file, importPackage, notify]);

  const onRequestRestore = useCallback((version: BrandingPackageVersion) => setPending({ kind: 'restore', version }), []);

  const runRestore = useCallback(
    (version: BrandingPackageVersion) => {
      setRestoringDigest(version.digest);
      restore.mutate(version.digest, {
        onSuccess: (restored) => {
          if (restored.applied) {
            notify({
              severity: 'success',
              message: t(
                'pages.admin.branding.package.restored',
                'Branding package restored. Users see it on their next page load.',
              ),
            });
            return;
          }
          notify({
            severity: 'error',
            message:
              reportSummary(restored) ??
              t('pages.admin.branding.package.restore.failed', 'The branding package could not be restored.'),
          });
        },
        onError: (error) =>
          notify({
            severity: 'error',
            message: failureMessage(
              error,
              t('pages.admin.branding.package.restore.failed', 'The branding package could not be restored.'),
            ),
          }),
        onSettled: () => setRestoringDigest(undefined),
      });
    },
    [notify, restore],
  );

  const onConfirmPending = useCallback(() => {
    if (pending === undefined) return;
    setPending(undefined);
    if (isDirty) discardDraft();
    if (pending.kind === 'import') openImport();
    else runRestore(pending.version);
  }, [discardDraft, isDirty, openImport, pending, runRestore]);

  const onCancelPending = useCallback(() => setPending(undefined), []);

  return {
    isDownloading: download.isPending,
    onDownload,
    importOpen,
    onRequestImport,
    onCloseImport,
    file,
    onPickFile,
    phase,
    report,
    checkError,
    canApply,
    applyBlockedReason,
    onApply,
    versions: versions.data ?? [],
    versionsLoading: versions.isPending,
    versionsError:
      versions.error === null || versions.error === undefined
        ? undefined
        : failureMessage(
            versions.error,
            t('pages.admin.branding.package.versions.failed', 'The kept packages could not be listed.'),
          ),
    restoringDigest,
    onRequestRestore,
    pending,
    onConfirmPending,
    onCancelPending,
  };
}
