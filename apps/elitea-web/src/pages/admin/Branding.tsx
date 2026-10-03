/**
 * Admin › Branding — the editor for the brand pack's database layer
 * (ADR-0024 WP4).
 *
 * ## What this page is
 *
 * White-labeling is a runtime setting here, not a build: the `branding`
 * section of the platform configuration holds twenty-one keys the resolver
 * lays over the mounted file pack and the product default, and the merged
 * pack is what `GET /api/v2/branding/bootstrap.js` serves to every user on
 * their next page load. The generic Configuration page could edit the same
 * rows as text; this page edits them as what they are — colours with a
 * derivation behind them, assets that must be uploaded, faces that must be
 * declared — and shows the result before it is saved.
 *
 * ## The three panels
 *
 *  - the FORM, grouped: identity, colour/type/shape, assets, font faces.
 *    Every field can inherit, and says what it would inherit and from where;
 *  - the PREVIEW: a sample surface in both schemes under a theme the real
 *    builder produced from the draft, with the derived swatches and a WCAG
 *    text-on-primary check (a warning only; nothing here blocks a save);
 *  - the LAYERS panel: which layers contribute and which decides each field;
 *  - the PACKAGE controls (WP9): download the current brand as a zip, import
 *    one with a dry run first, and restore a kept version. See
 *    `useBrandingPackage.ts` for how they respect the unsaved draft.
 *
 * ## Authorisation
 *
 * Every route this page calls is gated server-side on
 * `configuration.branding` (`internal/api/router.go`).
 * `window.admin_ui_config.permissions` hides the nav item and never gates
 * anything — see `./adminUiConfig`.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import LinearProgress from '@mui/material/LinearProgress';
import Snackbar from '@mui/material/Snackbar';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { BrandingAssetFields } from './BrandingAssetFields';
import { BrandingFontFacesEditor } from './BrandingFontFacesEditor';
import { BrandingIdentityFields } from './BrandingIdentityFields';
import { BrandingLayersPanel } from './BrandingLayersPanel';
import { BrandingNavGuard } from './BrandingNavGuard';
import { BrandingPackageConfirmDialog } from './BrandingPackageConfirmDialog';
import { BrandingPackageImportDialog } from './BrandingPackageImportDialog';
import { BrandingPackageVersions } from './BrandingPackageVersions';
import { BrandingPreview } from './BrandingPreview';
import { BrandingResetDialog } from './BrandingResetDialog';
import { BrandingStyleFields } from './BrandingStyleFields';
import { useAdminBrandingPage, type AdminBrandingPageState } from './useAdminBrandingPage';
import { useBrandingPackage, type BrandingPackageState } from './useBrandingPackage';

interface BrandingActionsProps {
  readonly state: AdminBrandingPageState;
  readonly pkg: BrandingPackageState;
}

function BrandingActions({ state, pkg }: BrandingActionsProps) {
  const busy = state.isSaving || state.uploadingKind !== undefined;
  const packageBusy = pkg.isDownloading || pkg.restoringDigest !== undefined;
  return (
    <Box sx={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap', alignItems: 'center' }}>
      <Button
        size="small"
        variant="secondary"
        disabled={busy || packageBusy}
        onClick={pkg.onDownload}
        data-testid="branding-package-download"
      >
        {pkg.isDownloading
          ? t('pages.admin.branding.package.download.busy', 'Preparing…')
          : t('pages.admin.branding.package.download', 'Download branding package')}
      </Button>
      <Button
        size="small"
        variant="secondary"
        disabled={busy || packageBusy}
        onClick={pkg.onRequestImport}
        data-testid="branding-package-import"
      >
        {t('pages.admin.branding.package.import', 'Import branding package…')}
      </Button>
      <Button
        size="small"
        variant="elitea"
        color="primary"
        disabled={!state.isDirty || busy}
        onClick={state.onSave}
        data-testid="branding-save"
      >
        {state.isSaving
          ? t('pages.admin.branding.action.saving', 'Saving…')
          : t('pages.admin.branding.action.save', 'Save')}
      </Button>
      <Button
        size="small"
        variant="secondary"
        disabled={!state.isDirty || busy}
        onClick={state.onDiscard}
        data-testid="branding-discard"
      >
        {t('pages.admin.branding.action.discard', 'Discard')}
      </Button>
      <Button
        size="small"
        variant="alarm"
        disabled={busy}
        onClick={state.onRequestReset}
        data-testid="branding-reset"
      >
        {t('pages.admin.branding.action.reset', 'Reset to defaults')}
      </Button>
    </Box>
  );
}

function BrandingForm({ state }: { readonly state: AdminBrandingPageState }) {
  const disabled = state.isSaving;
  const group = {
    values: state.values,
    basePack: state.basePack,
    layers: state.layers,
    disabled,
    fieldError: state.fieldError,
    onChange: state.onFieldChange,
  };
  return (
    <Box component="form" noValidate onSubmit={(event) => event.preventDefault()} sx={{ display: 'flex', flexDirection: 'column', gap: '1.5rem', minWidth: 0 }}>
      {state.fieldError !== undefined && state.fieldError.key === undefined ? (
        <Alert severity="error" data-testid="branding-save-error">
          {state.fieldError.message}
        </Alert>
      ) : null}
      <BrandingIdentityFields {...group} />
      <BrandingStyleFields {...group} />
      <BrandingAssetFields
        {...group}
        inheritedLogoEmail={state.effectiveLogoEmail}
        uploadingKind={state.uploadingKind}
        uploadError={state.uploadError}
        onUpload={state.onUploadAsset}
      />
      <BrandingFontFacesEditor
        faces={state.values.font_faces}
        effectiveFaces={state.effectiveFaces}
        disabled={disabled}
        uploading={state.uploadingKind === 'font'}
        uploadError={state.uploadError}
        fieldError={state.fieldError}
        onChange={state.onFontFacesChange}
        onUpload={(file, target) => state.onUploadAsset('font', file, target)}
      />
    </Box>
  );
}

export function AdminBranding() {
  const state = useAdminBrandingPage();
  const pkg = useBrandingPackage({
    isDirty: state.isDirty,
    discardDraft: state.onDiscard,
    notify: state.onNotify,
  });

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }}>
      <BrandingNavGuard />
      <Box sx={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: '1rem', flexWrap: 'wrap' }}>
        <Typography variant="headingLarge" component="h1">
          {t('pages.admin.branding.title', 'Branding')}
        </Typography>
        {state.isLoaded ? <BrandingActions state={state} pkg={pkg} /> : null}
      </Box>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.branding.subtitle',
          'Restyle the platform in your organisation’s name, colours and typography. Changes reach every user on their next page load; no rebuild or restart.',
        )}
      </Typography>

      {state.isLoading ? <LinearProgress aria-label={t('pages.admin.branding.loading', 'Loading branding')} /> : null}

      {state.loadError === undefined ? null : (
        <Alert severity="warning" data-testid="branding-load-error">
          {state.loadError}
        </Alert>
      )}

      {state.isLoaded ? (
        <Box
          sx={{
            display: 'grid',
            gridTemplateColumns: 'minmax(0, 3fr) minmax(18rem, 2fr)',
            gap: '2rem',
            alignItems: 'flex-start',
            '@media (max-width: 64rem)': { gridTemplateColumns: 'minmax(0, 1fr)' },
          }}
        >
          <BrandingForm state={state} />
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1.5rem', position: 'sticky', top: 0 }}>
            <BrandingPreview pack={state.previewPack} />
            <BrandingLayersPanel layers={state.layers} values={state.values} />
            <BrandingPackageVersions
              versions={pkg.versions}
              isLoading={pkg.versionsLoading}
              error={pkg.versionsError}
              restoringDigest={pkg.restoringDigest}
              disabled={state.isSaving || state.uploadingKind !== undefined}
              onRestore={pkg.onRequestRestore}
            />
          </Box>
        </Box>
      ) : null}

      <BrandingResetDialog
        open={state.resetOpen}
        onCancel={state.onCancelReset}
        onConfirm={state.onConfirmReset}
      />
      <BrandingPackageImportDialog state={pkg} />
      <BrandingPackageConfirmDialog
        pending={pkg.pending}
        isDirty={state.isDirty}
        onCancel={pkg.onCancelPending}
        onConfirm={pkg.onConfirmPending}
      />

      <Snackbar
        open={state.toast !== undefined}
        autoHideDuration={6000}
        onClose={state.onDismissToast}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
      >
        {state.toast === undefined ? undefined : (
          <Alert
            severity={state.toast.severity}
            onClose={state.onDismissToast}
            data-testid={`branding-toast-${state.toast.severity}`}
          >
            {state.toast.message}
          </Alert>
        )}
      </Snackbar>
    </DrawerPage>
  );
}
