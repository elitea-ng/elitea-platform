/**
 * Admin › Secrets — the GLOBAL secret vault (unit A14, issue #200).
 *
 * Reference (read-only): `frontends/admin_ui/frontend/src/pages/SecretsPage/`.
 * Rewritten against this app's stack: Redux Toolkit → TanStack Query
 * (`./api/adminSecretsApi`), axios → `eliteaFetch`, react-router → TanStack
 * Router (`./router`), MUI 7 → 9. State and handlers live in
 * `./useAdminSecretsPage`; the value cell is reused from
 * `features/settings/ui/secrets`.
 *
 * ## This page addresses a different store from Settings › Secrets
 *
 * Settings › Secrets edits ONE project's vault (`project-<id>`). This edits the
 * `admin` row, whose contents pylon merges into EVERY project's
 * `{{secret.…}}` resolution. A write here is platform-wide. The two surfaces
 * share components and share nothing else — different endpoints, different
 * bodies, different query keys.
 *
 * ## What is real and what is not
 *
 * All two reads and all three writes are REAL. Before this unit the
 * `administration` mode of the secrets routes answered **501** — deliberately,
 * and correctly: every method on the project handler is keyed by the
 * `{projectID}` segment, and admin_ui sends the placeholder `0` there, so
 * serving this page from it would have read and WRITTEN `project-0`'s vault.
 * `services/elitea-main/internal/api/v2/secrets/admin.go` is the separate
 * handler that 501 was pointing at, covered by write-then-re-read tests that
 * also assert the project vaults did not move.
 *
 * One control is deliberately absent rather than disabled: the reference has no
 * "hide" affordance either, and pylon's admin hide endpoint answers 401 with
 * "There are no hidden secrets in administration mode". The server says the same
 * thing; there is nothing to render.
 *
 * The Internal tab is read-only for everyone — see `./useAdminSecretsPage` for
 * what that classification is and why those rows are shown rather than hidden.
 *
 * ## Authorisation
 *
 * `window.admin_ui_config.permissions` is presentation state and never a gate —
 * see `./adminUiConfig`. Every read and every write is authorised server-side on
 * each request against `configuration.secrets.secret.{view,create,edit,delete}`
 * resolved from `auth_core__user_role` in `administration` mode.
 */
import AddIcon from '@mui/icons-material/Add';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import Typography from '@mui/material/Typography';

import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminSecretDialog } from './AdminSecretDialog';
import { AdminSecretsTable } from './AdminSecretsTable';
import { useAdminSecretsPage, type AdminSecretsPageState } from './useAdminSecretsPage';


/**
 * A header action keeps its label on one line and does not give up width:
 * the search field beside it shrinks first. Without this the 28px pill
 * stacked "Create secret" on two lines.
 */
const headerActionSx = { whiteSpace: 'nowrap', flexShrink: 0 } as const;

function savedText(message: string): string {
  if (message === 'created') return t('pages.admin.secrets.saved.created', 'Secret created.');
  if (message === 'updated') return t('pages.admin.secrets.saved.updated', 'Secret updated.');
  return t('pages.admin.secrets.saved.deleted', 'Secret deleted.');
}

function errorText(message: string): string {
  if (message === 'reveal') {
    return t('pages.admin.secrets.error.reveal', 'Failed to read the secret value.');
  }
  if (message === 'delete') return t('pages.admin.secrets.error.delete', 'Failed to delete the secret.');
  return message;
}

function SecretsBody({ state }: { readonly state: AdminSecretsPageState }) {
  if (state.isError) {
    return (
      <Alert severity="warning" data-testid="admin-secrets-unavailable">
        {state.unavailableReason ??
          t('pages.admin.secrets.error.load', 'Failed to load the global secrets.')}
      </Alert>
    );
  }
  return (
    <AdminSecretsTable
      secrets={state.rows}
      isLoading={state.isFetching}
      onReveal={state.onReveal}
      canReveal={state.canReveal}
      onEdit={state.onEdit}
      onDelete={state.onDelete}
    />
  );
}

export function AdminSecrets() {
  const state = useAdminSecretsPage();

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }}>
      <Box
        sx={{
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'center',
          gap: '1rem',
          flexWrap: 'wrap',
        }}
      >
        <Typography variant="headingLarge" component="h1">
          {t('pages.admin.secrets.title', 'Secrets')}
        </Typography>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          <SimpleSearchBar
            value={state.search}
            onChange={state.onSearchChange}
            placeholder={t('pages.admin.secrets.search', 'Search by name')}
            data-testid="admin-secrets-search"
          />
          {state.onCreate ? (
            <Button variant="elitea" color="primary" size="small" startIcon={<AddIcon />} onClick={state.onCreate} sx={headerActionSx}>
              {t('pages.admin.secrets.action.create', 'Create secret')}
            </Button>
          ) : null}
        </Box>
      </Box>

      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.secrets.subtitle',
          'These secrets are shared across every project on this deployment.',
        )}
      </Typography>

      <Tabs value={state.activeTab} onChange={state.onTabChange} sx={{ minHeight: '2.5rem' }}>
        <Tab
          label={`${t('pages.admin.secrets.tab.user', 'User Secrets')} (${state.counts.user})`}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
        <Tab
          label={`${t('pages.admin.secrets.tab.internal', 'Internal')} (${state.counts.internal})`}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
      </Tabs>

      {state.activeTab === 1 ? (
        <Alert severity="info" data-testid="admin-secrets-internal-notice">
          {t(
            'pages.admin.secrets.internalNotice',
            'These secrets are written by the platform itself and are read-only here.',
          )}
        </Alert>
      ) : null}

      {state.errorMessage !== '' ? (
        <Alert severity="error" onClose={state.onDismissError}>
          {errorText(state.errorMessage)}
        </Alert>
      ) : null}
      {state.savedMessage !== '' ? (
        <Alert severity="success" onClose={state.onDismissSaved}>
          {savedText(state.savedMessage)}
        </Alert>
      ) : null}

      <Box sx={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
        <SecretsBody state={state} />
      </Box>

      <AdminSecretDialog
        open={state.dialogOpen}
        editingName={state.editingName}
        existingNames={state.allNames}
        isSaving={state.isSaving}
        serverError={state.saveError}
        onClose={state.onDialogClose}
        onSubmit={state.onDialogSubmit}
      />

      {/*
        Retyping the name is required. A global secret is resolved by every
        project on the deployment, so deleting the wrong one breaks agents that
        have nothing to do with whoever clicked.
      */}
      <DeleteEntityModal
        open={state.deleteName !== undefined}
        onClose={state.onDeleteCancel}
        onConfirm={state.onDeleteConfirm}
        confirming={state.isDeleting}
        name={state.deleteName ?? ''}
        shouldRequestInputName
        copy={{ title: t('pages.admin.secrets.deleteModal.title', 'Delete secret') }}
        data-testid="admin-secrets-delete-modal"
      />
    </DrawerPage>
  );
}
