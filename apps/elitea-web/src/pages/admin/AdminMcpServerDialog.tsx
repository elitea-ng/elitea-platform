/**
 * Create / Edit dialog for one pre-built MCP server (the catalogue entry).
 *
 * One component for both, for the reason `./AdminSecretDialog.tsx` gives: the
 * two differ only in whether the key is editable, and splitting them would
 * duplicate every field, the validation and the error surface.
 *
 * ## The client secret is never pre-filled
 *
 * The server never sends it — a read renders a mask — so there is nothing to
 * pre-fill with, and fetching the plaintext to display it would put a
 * credential on screen because a dialog opened.
 *
 * That makes "unchanged" the DEFAULT on edit: leaving the field untouched
 * sends no `client_secret` at all and the sealed one stays. Clearing it is an
 * explicit act, and the checkbox is what makes the difference visible instead
 * of hiding it in an empty text field.
 *
 * ## Client-side validation is a courtesy, not a gate
 *
 * The server refuses a blank display name, a negative timeout and a non-HTTP
 * transport independently (`internal/api/v2/admin/mcp_prebuilt.go`). These
 * checks save a round trip; the server's own words are what this dialog renders
 * when the two disagree.
 */
import { useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Checkbox from '@mui/material/Checkbox';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

import {
  initialForm,
  keyHelperText,
  normalizeCatalogueKey,
  parseMcpHeadersYaml,
  parseMcpYamlMapping,
  resolveSecretForSave,
  validateDraft,
  type McpServerForm,
} from './adminMcpServerForm';
import type { AdminMcpServer, AdminMcpServerDraft } from './api/adminMcpServersApi';

export interface AdminMcpServerDialogProps {
  readonly open: boolean;
  /** `undefined` ⇒ create; an entry ⇒ edit it. */
  readonly editing: AdminMcpServer | undefined;
  /** Every key currently catalogued, for the duplicate check on create. */
  readonly existingKeys: ReadonlySet<string>;
  readonly isSaving: boolean;
  /** The server's own words when the last attempt was refused. */
  readonly serverError: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (draft: AdminMcpServerDraft) => void;
}

export function AdminMcpServerDialog({
  open,
  editing,
  existingKeys,
  isSaving,
  serverError,
  onClose,
  onSubmit,
}: AdminMcpServerDialogProps) {
  const isEdit = editing !== undefined;
  const [form, setForm] = useState<McpServerForm>(() => initialForm(undefined));
  const [localError, setLocalError] = useState('');

  useEffect(() => {
    if (!open) return;
    setForm(initialForm(editing));
    setLocalError('');
  }, [open, editing]);

  const update = <K extends keyof McpServerForm>(key: K, value: McpServerForm[K]): void => {
    setForm((previous) => ({ ...previous, [key]: value }));
  };

  const handleSubmit = (): void => {
    const problem = validateDraft(
      form.displayName,
      form.timeout,
      form.headersYaml,
      form.configSchemaYaml,
      isEdit,
      existingKeys,
    );
    if (problem !== undefined) {
      setLocalError(problem);
      return;
    }
    const name = form.displayName.trim();
    setLocalError('');
    onSubmit({
      // On edit the key is FIXED. Renaming would orphan the sealed secret and
      // every toolkit whose type resolves against the old key, so a rename is a
      // delete plus a create — which is what the server's key-per-row model
      // says too.
      key: isEdit ? editing.key : normalizeCatalogueKey(name),
      displayName: name,
      url: form.url.trim(),
      baseUrl: form.baseUrl.trim(),
      clientId: form.clientId.trim(),
      clientSecret: resolveSecretForSave(form.clientSecret, form.clearSecret),
      timeout: form.timeout.trim() === '' ? 0 : Number(form.timeout),
      headers: parseMcpHeadersYaml(form.headersYaml),
      configSchema: parseMcpYamlMapping(form.configSchemaYaml),
      enabled: form.enabled,
    });
  };

  const error = localError !== '' ? localError : serverError;
  const hasStoredSecret = editing?.client_secret !== undefined && editing.client_secret !== '';

  return (
    <Dialog open={open} onClose={onClose} maxWidth="sm" fullWidth data-testid="admin-mcp-server-dialog">
      <DialogTitle>
        {isEdit
          ? t('pages.admin.mcpServers.dialog.editTitle', 'Edit MCP server')
          : t('pages.admin.mcpServers.dialog.createTitle', 'Add MCP server')}
      </DialogTitle>
      <DialogContent
        sx={{ display: 'flex', flexDirection: 'column', gap: '1rem', paddingTop: '0.5rem' }}
      >
        {error !== undefined && error !== '' ? (
          <Alert severity="error" data-testid="admin-mcp-server-dialog-error">
            {error}
          </Alert>
        ) : null}

        <TextField
          label={t('pages.admin.mcpServers.dialog.name', 'Display name')}
          value={form.displayName}
          onChange={(event) => {
            update('displayName', event.target.value);
          }}
          disabled={isSaving}
          fullWidth
          size="small"
          // The derived key is shown while typing, because it — not the display
          // name — is what a toolkit type is matched against at run time.
          helperText={keyHelperText(isEdit ? editing.key : undefined, form.displayName)}
        />

        <TextField
          label={t('pages.admin.mcpServers.dialog.url', 'Server URL')}
          value={form.url}
          onChange={(event) => {
            update('url', event.target.value);
          }}
          disabled={isSaving}
          fullWidth
          size="small"
          helperText={t(
            'pages.admin.mcpServers.dialog.urlHelp',
            'The streamable-HTTP MCP endpoint. Local stdio servers cannot be catalogued.',
          )}
        />

        <TextField
          label={t('pages.admin.mcpServers.dialog.baseUrl', 'Base URL (optional)')}
          value={form.baseUrl}
          onChange={(event) => {
            update('baseUrl', event.target.value);
          }}
          disabled={isSaving}
          fullWidth
          size="small"
        />

        <TextField
          label={t('pages.admin.mcpServers.dialog.clientId', 'OAuth client ID (optional)')}
          value={form.clientId}
          onChange={(event) => {
            update('clientId', event.target.value);
          }}
          disabled={isSaving}
          fullWidth
          size="small"
        />

        <McpSecretFields
          form={form}
          hasStoredSecret={hasStoredSecret}
          isSaving={isSaving}
          onUpdate={update}
        />

        <TextField
          label={t('pages.admin.mcpServers.dialog.timeout', 'Timeout in seconds (optional)')}
          value={form.timeout}
          onChange={(event) => {
            update('timeout', event.target.value);
          }}
          disabled={isSaving}
          fullWidth
          size="small"
          inputMode="numeric"
        />

        <TextField
          label={t('pages.admin.mcpServers.dialog.headers', 'Headers (YAML mapping)')}
          value={form.headersYaml}
          onChange={(event) => {
            update('headersYaml', event.target.value);
          }}
          disabled={isSaving}
          fullWidth
          multiline
          minRows={3}
          size="small"
          helperText={t(
            'pages.admin.mcpServers.dialog.headersHelp',
            'Header values may use declared placeholders, for example Bearer {api_token}.',
          )}
        />

        <TextField
          label={t('pages.admin.mcpServers.dialog.configSchema', 'Project parameter schema (YAML mapping)')}
          value={form.configSchemaYaml}
          onChange={(event) => {
            update('configSchemaYaml', event.target.value);
          }}
          disabled={isSaving}
          fullWidth
          multiline
          minRows={5}
          size="small"
          helperText={t(
            'pages.admin.mcpServers.dialog.configSchemaHelp',
            'Define properties with type, description, required, default, and secret metadata. Main validates every template before saving.',
          )}
        />

        <FormControlLabel
          control={
            <Checkbox
              checked={form.enabled}
              onChange={(event) => {
                update('enabled', event.target.checked);
              }}
              disabled={isSaving}
              data-testid="admin-mcp-server-enabled"
            />
          }
          label={t('pages.admin.mcpServers.dialog.enabled', 'Offer this server to projects')}
        />
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={isSaving} sx={{ textTransform: 'none' }}>
          {t('pages.admin.mcpServers.dialog.cancel', 'Cancel')}
        </Button>
        <Button
          variant="contained"
          onClick={handleSubmit}
          disabled={isSaving}
          sx={{ textTransform: 'none' }}
        >
          {isSaving
            ? t('pages.admin.mcpServers.dialog.saving', 'Saving…')
            : t('pages.admin.mcpServers.dialog.save', 'Save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

/**
 * The client-secret field and its clear checkbox.
 *
 * Extracted because these two controls are the ones that decide the tri-state
 * the server reads, and keeping them together makes that relationship visible:
 * ticking "remove" disables the field and empties it, so a stale keystroke
 * cannot resurrect a secret the operator chose to delete.
 */
function McpSecretFields({
  form,
  hasStoredSecret,
  isSaving,
  onUpdate,
}: {
  readonly form: McpServerForm;
  readonly hasStoredSecret: boolean;
  readonly isSaving: boolean;
  readonly onUpdate: <K extends keyof McpServerForm>(key: K, value: McpServerForm[K]) => void;
}) {
  return (
    <>
      <TextField
        label={t('pages.admin.mcpServers.dialog.clientSecret', 'OAuth client secret')}
        type="password"
        value={form.clientSecret}
        onChange={(event) => {
          onUpdate('clientSecret', event.target.value);
        }}
        disabled={isSaving || form.clearSecret}
        fullWidth
        size="small"
        autoComplete="new-password"
        helperText={
          hasStoredSecret
            ? t(
                'pages.admin.mcpServers.dialog.secretStored',
                'A secret is stored. Leave blank to keep it; it is sealed in the platform vault and never shown.',
              )
            : t(
                'pages.admin.mcpServers.dialog.secretNew',
                'Sealed into the platform vault. It is never returned by any endpoint.',
              )
        }
      />

      {hasStoredSecret ? (
        <FormControlLabel
          control={
            <Checkbox
              checked={form.clearSecret}
              onChange={(event) => {
                onUpdate('clearSecret', event.target.checked);
                if (event.target.checked) onUpdate('clientSecret', '');
              }}
              disabled={isSaving}
              data-testid="admin-mcp-server-clear-secret"
            />
          }
          label={t('pages.admin.mcpServers.dialog.clearSecret', 'Remove the stored client secret')}
        />
      ) : null}
    </>
  );
}
