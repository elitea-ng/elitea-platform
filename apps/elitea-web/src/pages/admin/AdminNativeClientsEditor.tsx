/**
 * Admin › Configuration › Native clients — the native client registry
 * (ADR-0025 WP2).
 *
 * The section used to render only its `unavailable_reason` ("native clients are
 * registered on their own editor…"), which was true of the plugin-config value
 * endpoints and pointed at an editor this app did not have. The server already
 * declared `managed_surface: native_clients`; this is the editor it names, and
 * `./Configuration.tsx` reaches it through that word, never a section id.
 *
 * ## Two writes destroy sessions, and both confirm
 *
 * Disabling a client and removing it each revoke EVERY device of it, in the
 * same transaction as the write (`internal/nativeauth/store.go`). Enabling the
 * client again resurrects none of them. Both confirmations name the live
 * device count so the operator sees the blast radius before choosing it, and
 * the result says how many devices the server actually signed out.
 */
import { useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import LinearProgress from '@mui/material/LinearProgress';
import Typography from '@mui/material/Typography';

import type { NativeClient } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { AdminNativeClientDialog } from './AdminNativeClientDialog';
import { NativeClientTable } from './AdminNativeClientTable';
import {
  NO_FIELD_ERRORS,
  placeNativeClientReasons,
  type NativeClientFieldErrors,
} from './adminNativeClientForm';
import {
  isNativeServerAbsent,
  nativeClientFailure,
  useAdminNativeClients,
  useDeleteNativeClient,
  useSaveNativeClient,
  type NativeClientDraft,
  type NativeClientWriteOutcome,
} from './api/adminNativeClientsApi';

/** A pending destructive action, waiting on its confirmation. */
type PendingAction = { readonly kind: 'disable' | 'remove'; readonly client: NativeClient };

function clientName(client: NativeClient): string {
  return client.display_name !== '' ? client.display_name : client.client_id;
}

/** The sentence for a refusal that names no field, in the server's words when it gave some. */
function refusalSentence(error: unknown, fallback: string): string {
  const failure = nativeClientFailure(error);
  return failure.message ?? failure.code ?? fallback;
}

function outcomeSentence(outcome: NativeClientWriteOutcome): string {
  return outcome.revokedDevices > 0
    ? t('pages.admin.nativeClients.result.revoked', 'Saved. {{count}} signed-in devices were signed out.', {
        count: outcome.revokedDevices,
      })
    : t('pages.admin.nativeClients.result.saved', 'Saved.');
}

function confirmBody(action: PendingAction): string {
  const name = clientName(action.client);
  const count = action.client.active_devices;
  if (action.kind === 'disable') {
    return t(
      'pages.admin.nativeClients.disable.body',
      'Disabling “{{name}}” signs out all {{count}} of its signed-in devices now; each is wiped the next time it contacts the server. Enabling it again does not restore them.',
      { name, count },
    );
  }
  return action.client.overridden_file
    ? t(
        'pages.admin.nativeClients.remove.bodyOverride',
        'Removing “{{name}}” signs out all {{count}} of its signed-in devices. The entry for this ID in the NATIVE_CLIENTS_PATH file applies again afterwards.',
        { name, count },
      )
    : t(
        'pages.admin.nativeClients.remove.body',
        'Removing “{{name}}” signs out all {{count}} of its signed-in devices, and the app can no longer sign anyone in.',
        { name, count },
      );
}

function ConfirmDialog({
  action,
  busy,
  onCancel,
  onConfirm,
}: {
  readonly action: PendingAction | undefined;
  readonly busy: boolean;
  readonly onCancel: () => void;
  readonly onConfirm: () => void;
}) {
  const isDisable = action?.kind === 'disable';
  return (
    <Dialog open={action !== undefined} onClose={onCancel} maxWidth="xs" fullWidth data-testid="native-client-confirm-dialog">
      <DialogTitle>
        {isDisable
          ? t('pages.admin.nativeClients.disable.title', 'Disable native client')
          : t('pages.admin.nativeClients.remove.title', 'Remove native client')}
      </DialogTitle>
      <DialogContent>
        <Typography variant="bodyMedium">{action === undefined ? '' : confirmBody(action)}</Typography>
      </DialogContent>
      <DialogActions>
        <Button onClick={onCancel} disabled={busy} sx={{ textTransform: 'none' }}>
          {t('pages.admin.nativeClients.confirm.cancel', 'Cancel')}
        </Button>
        <Button
          variant="elitea"
          color="alarm"
          onClick={onConfirm}
          disabled={busy}
          sx={{ textTransform: 'none' }}
          data-testid="native-client-confirm"
        >
          {isDisable
            ? t('pages.admin.nativeClients.disable.confirm', 'Disable')
            : t('pages.admin.nativeClients.remove.confirm', 'Remove')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

function LoadAlert({ error }: { readonly error: unknown }) {
  if (error == null) return null;
  return (
    <Alert severity="warning" data-testid="admin-native-clients-error">
      {isNativeServerAbsent(error)
        ? t(
            'pages.admin.nativeClients.error.absent',
            'This deployment does not serve native sign-in, so no native client can be registered here.',
          )
        : refusalSentence(error, t('pages.admin.nativeClients.error.load', 'Failed to load the native clients.'))}
    </Alert>
  );
}

/** The dialog's state: what it edits and why the last save was refused. */
interface DialogState {
  readonly open: boolean;
  readonly editing: NativeClient | undefined;
  readonly serverError: string | undefined;
  readonly fieldErrors: NativeClientFieldErrors;
}

const CLOSED_DIALOG: DialogState = { open: false, editing: undefined, serverError: undefined, fieldErrors: NO_FIELD_ERRORS };

export function AdminNativeClientsEditor() {
  const listQuery = useAdminNativeClients();
  const saveMutation = useSaveNativeClient();
  const deleteMutation = useDeleteNativeClient();

  const [dialog, setDialog] = useState<DialogState>(CLOSED_DIALOG);
  const [pending, setPending] = useState<PendingAction | undefined>(undefined);
  const [notice, setNotice] = useState<{ severity: 'success' | 'error'; text: string } | undefined>(undefined);

  const clients = useMemo(() => listQuery.data ?? [], [listQuery.data]);

  const busyIds = useMemo(() => {
    const ids = new Set<string>();
    if (saveMutation.isPending && saveMutation.variables) ids.add(saveMutation.variables.clientId);
    if (deleteMutation.isPending && deleteMutation.variables) ids.add(deleteMutation.variables);
    return ids;
  }, [saveMutation.isPending, saveMutation.variables, deleteMutation.isPending, deleteMutation.variables]);

  const handleSubmit = (draft: NativeClientDraft): void => {
    setDialog((previous) => ({ ...previous, serverError: undefined, fieldErrors: NO_FIELD_ERRORS }));
    saveMutation.mutate(draft, {
      onSuccess: (outcome) => {
        setDialog(CLOSED_DIALOG);
        setNotice({ severity: 'success', text: outcomeSentence(outcome) });
      },
      // The dialog STAYS OPEN, holding what was typed, with each reason placed
      // beside the field (or the redirect URI) it refused.
      onError: (error: unknown) => {
        const failure = nativeClientFailure(error);
        const fieldErrors = placeNativeClientReasons(failure.reasons, draft.redirectUris);
        const hasFieldErrors = Object.keys(failure.reasons).length > 0;
        setDialog((previous) => ({
          ...previous,
          fieldErrors,
          serverError: hasFieldErrors
            ? undefined
            : refusalSentence(error, t('pages.admin.nativeClients.error.save', 'Failed to save that native client.')),
        }));
      },
    });
  };

  const writeEnabled = (client: NativeClient, enabled: boolean): void => {
    setNotice(undefined);
    saveMutation.mutate(
      {
        clientId: client.client_id,
        displayName: client.display_name,
        redirectUris: client.redirect_uris,
        enabled,
        minClientVersion: client.min_client_version,
      },
      {
        onSuccess: (outcome) => {
          setPending(undefined);
          setNotice({ severity: 'success', text: outcomeSentence(outcome) });
        },
        onError: (error: unknown) => {
          setPending(undefined);
          setNotice({
            severity: 'error',
            text: refusalSentence(error, t('pages.admin.nativeClients.error.save', 'Failed to save that native client.')),
          });
        },
      },
    );
  };

  /** The row switch: enabling is immediate, disabling confirms first. */
  const handleToggle = (client: NativeClient): void => {
    if (client.enabled) {
      setPending({ kind: 'disable', client });
      return;
    }
    writeEnabled(client, true);
  };

  const handleConfirm = (): void => {
    if (pending === undefined) return;
    if (pending.kind === 'disable') {
      writeEnabled(pending.client, false);
      return;
    }
    setNotice(undefined);
    deleteMutation.mutate(pending.client.client_id, {
      onSuccess: (outcome) => {
        setPending(undefined);
        setNotice({ severity: 'success', text: outcomeSentence(outcome) });
      },
      onError: (error: unknown) => {
        setPending(undefined);
        setNotice({
          severity: 'error',
          text: refusalSentence(error, t('pages.admin.nativeClients.error.delete', 'Failed to remove that native client.')),
        });
      },
    });
  };

  const showEmpty = !listQuery.isLoading && listQuery.error == null && clients.length === 0;

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }} data-testid="admin-native-clients-editor">
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.nativeClients.description',
          'The mobile and desktop apps that may sign users in to this deployment. Users approve each new device from a signed-in browser, and can see and revoke their devices under Settings › Devices.',
        )}
      </Typography>

      {listQuery.isLoading ? <LinearProgress /> : null}
      <LoadAlert error={listQuery.error} />
      {notice !== undefined ? (
        <Alert severity={notice.severity} onClose={() => setNotice(undefined)} data-testid="admin-native-clients-notice">
          {notice.text}
        </Alert>
      ) : null}

      <Box>
        <Button
          size="small"
          variant="elitea"
          color="primary"
          onClick={() => setDialog({ ...CLOSED_DIALOG, open: true })}
          sx={{ textTransform: 'none' }}
          data-testid="admin-native-clients-add"
        >
          {t('pages.admin.nativeClients.add', 'Register native client')}
        </Button>
      </Box>

      {showEmpty ? (
        <Typography variant="bodyMedium" color="text.secondary" data-testid="admin-native-clients-empty">
          {t(
            'pages.admin.nativeClients.empty',
            'No native client is registered, so no mobile or desktop app can sign in to this deployment.',
          )}
        </Typography>
      ) : null}

      {clients.length > 0 ? (
        <NativeClientTable
          clients={clients}
          busyIds={busyIds}
          onEdit={(client) => setDialog({ ...CLOSED_DIALOG, open: true, editing: client })}
          onToggleEnabled={handleToggle}
          onRemove={(client) => setPending({ kind: 'remove', client })}
        />
      ) : null}

      <AdminNativeClientDialog
        open={dialog.open}
        editing={dialog.editing}
        isSaving={saveMutation.isPending}
        serverError={dialog.serverError}
        fieldErrors={dialog.fieldErrors}
        onClose={() => setDialog(CLOSED_DIALOG)}
        onSubmit={handleSubmit}
      />

      <ConfirmDialog
        action={pending}
        busy={saveMutation.isPending || deleteMutation.isPending}
        onCancel={() => setPending(undefined)}
        onConfirm={handleConfirm}
      />
    </Box>
  );
}
