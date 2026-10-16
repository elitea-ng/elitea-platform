/**
 * Admin › Configuration › MCP Servers — the pre-built MCP catalogue editor.
 *
 * This is the section that used to render only a refusal. The refusal was
 * accurate about the plugin-config VALUE endpoints, which still cannot serve
 * this section, and stopped being the whole truth when the catalogue acquired a
 * surface of its own (`./api/adminMcpServersApi`).
 *
 * The page reaches this component through a server-declared `managed_surface`,
 * never through a hardcoded section id — see `./Configuration.tsx`.
 *
 * ## What an operator can and cannot see
 *
 * A stored client secret renders as the mask the server sent. There is no
 * "reveal": the plaintext is sealed in the platform vault's hidden bucket and
 * no endpoint returns it, so there is nothing to reveal and a control offering
 * to would be a lie.
 *
 * ## Delete confirms, because it destroys a credential
 *
 * Removing an entry also removes its sealed secret, which cannot be recovered
 * from this screen or any other. An operator who only wants to stop offering a
 * server should clear the "Offer this server" checkbox instead, which keeps the
 * secret — the confirmation says so.
 */
import { useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import LinearProgress from '@mui/material/LinearProgress';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { AdminMcpServerDialog } from './AdminMcpServerDialog';
import { configFailureReason } from './api/adminConfigurationApi';
import {
  useAdminMcpServers,
  useDeleteAdminMcpServer,
  useSaveAdminMcpServer,
  type AdminMcpServer,
  type AdminMcpServerDraft,
} from './api/adminMcpServersApi';

/**
 * The load and action alerts.
 *
 * Extracted so the editor itself stays under the complexity gate. Both render
 * the SERVER's own sentence when it gave one: on this surface the refusals are
 * specific and actionable ("only http MCP servers can be catalogued here…"),
 * and collapsing them into "Failed" would discard the only words that say which.
 */
function McpCatalogueAlerts({
  loadError,
  actionError,
  onDismissAction,
}: {
  readonly loadError: unknown;
  readonly actionError: string | undefined;
  readonly onDismissAction: () => void;
}) {
  return (
    <>
      {loadError != null ? (
        <Alert severity="warning" data-testid="admin-mcp-servers-error">
          {configFailureReason(loadError) ??
            t('pages.admin.mcpServers.error.load', 'Failed to load the MCP server catalogue.')}
        </Alert>
      ) : null}

      {actionError !== undefined ? (
        <Alert severity="error" onClose={onDismissAction} data-testid="admin-mcp-servers-action-error">
          {actionError === 'delete'
            ? t('pages.admin.mcpServers.error.delete', 'Failed to remove that MCP server.')
            : actionError}
        </Alert>
      ) : null}
    </>
  );
}

/**
 * The catalogue table.
 *
 * Extracted from the editor so each piece stays under the complexity gate, and
 * because a row's rendering — mask, status, actions — is the part most likely
 * to change independently of the page's state machine.
 */
function McpServerTable({
  servers,
  onEdit,
  onRemove,
}: {
  readonly servers: readonly AdminMcpServer[];
  readonly onEdit: (server: AdminMcpServer) => void;
  readonly onRemove: (server: AdminMcpServer) => void;
}) {
  return (
    <TableContainer>
      {/* Named, so the table has an accessible name for a screen reader and a
          stable handle for the journey and visual specs — the same shape as the
          admin Schedules table. */}
      <Table
        size="small"
        aria-label={t('pages.admin.mcpServers.tableLabel', 'MCP servers')}
        data-testid="admin-mcp-servers-table"
      >
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.mcpServers.column.name', 'Name')}</TableCell>
            <TableCell>{t('pages.admin.mcpServers.column.url', 'Server URL')}</TableCell>
            <TableCell>{t('pages.admin.mcpServers.column.secret', 'Client secret')}</TableCell>
            <TableCell>{t('pages.admin.mcpServers.column.status', 'Status')}</TableCell>
            <TableCell align="right">
              {t('pages.admin.mcpServers.column.actions', 'Actions')}
            </TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {servers.map((server) => (
            <TableRow key={server.key} hover>
              <TableCell>
                <Typography variant="bodyMedium">{server.display_name}</Typography>
                <Typography variant="bodySmall" color="text.secondary">
                  {server.key}
                </Typography>
              </TableCell>
              <TableCell sx={{ wordBreak: 'break-all' }}>
                {server.url}
                {server.token_url_required === true ? (
                  <Chip
                    size="small"
                    color="warning"
                    variant="outlined"
                    sx={{ marginLeft: '0.5rem' }}
                    data-testid={`admin-mcp-server-token-url-required-${server.key}`}
                    label={t('pages.admin.mcpServers.tokenUrlRequired', 'Token URL required')}
                  />
                ) : null}
              </TableCell>
              <TableCell>
                {server.client_secret !== undefined && server.client_secret !== '' ? (
                  // The mask, exactly as the server sent it. There is no reveal
                  // control because there is nothing to reveal.
                  <Typography variant="bodySmall">{server.client_secret}</Typography>
                ) : (
                  <Typography variant="bodySmall" color="text.secondary">
                    {t('pages.admin.mcpServers.noSecret', 'None')}
                  </Typography>
                )}
              </TableCell>
              <TableCell>
                <Chip
                  size="small"
                  label={
                    server.enabled
                      ? t('pages.admin.mcpServers.enabled', 'Offered')
                      : t('pages.admin.mcpServers.disabled', 'Withdrawn')
                  }
                  color={server.enabled ? 'success' : 'default'}
                  variant="outlined"
                />
              </TableCell>
              <TableCell align="right">
                <Button
            variant="elitea" color="tertiary"
                  size="small"
                  onClick={() => {
                    onEdit(server);
                  }}
                  sx={{ textTransform: 'none' }}
                >
                  {t('pages.admin.mcpServers.edit', 'Edit')}
                </Button>
                <Button
            variant="elitea" color="alarm"
                  size="small"
                                    onClick={() => {
                    onRemove(server);
                  }}
                  sx={{ textTransform: 'none' }}
                >
                  {t('pages.admin.mcpServers.remove', 'Remove')}
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}

/**
 * The delete confirmation's wording.
 *
 * Interpolated through `t`'s own options rather than by `String.replace` on the
 * fallback: the fallback is only used when the bundle has no entry, so a
 * `replace` would silently stop substituting the moment the string was
 * translated.
 */
function deleteWarning(name: string): string {
  return t(
    'pages.admin.mcpServers.delete.body',
    'This removes “{{name}}” from the catalogue and deletes its stored client secret, which cannot be recovered. To stop offering it while keeping the secret, edit it and clear “Offer this server to projects” instead.',
    { name },
  );
}

export function AdminMcpServersEditor() {
  const listQuery = useAdminMcpServers();
  const saveMutation = useSaveAdminMcpServer();
  const deleteMutation = useDeleteAdminMcpServer();

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<AdminMcpServer | undefined>(undefined);
  const [pendingDelete, setPendingDelete] = useState<AdminMcpServer | undefined>(undefined);
  const [saveError, setSaveError] = useState<string | undefined>(undefined);
  const [actionError, setActionError] = useState<string | undefined>(undefined);

  // `listQuery.data ?? []` would allocate a new array every render, so the
  // memo below would never hit. Memoising the fallback is what makes the
  // dependency stable.
  const servers = useMemo(() => listQuery.data ?? [], [listQuery.data]);
  const existingKeys = useMemo(() => new Set(servers.map((server) => server.key)), [servers]);

  const openCreate = (): void => {
    setEditing(undefined);
    setSaveError(undefined);
    setDialogOpen(true);
  };

  const openEdit = (server: AdminMcpServer): void => {
    setEditing(server);
    setSaveError(undefined);
    setDialogOpen(true);
  };

  const handleSubmit = (draft: AdminMcpServerDraft): void => {
    setSaveError(undefined);
    saveMutation.mutate(draft, {
      onSuccess: () => {
        setDialogOpen(false);
        setEditing(undefined);
      },
      // The dialog STAYS OPEN on failure, holding what was typed. Closing it
      // would discard the operator's input along with the reason it was
      // refused.
      onError: (error: unknown) => {
        setSaveError(configFailureReason(error) ?? 'save');
      },
    });
  };

  const handleDelete = (): void => {
    if (pendingDelete === undefined) return;
    setActionError(undefined);
    deleteMutation.mutate(pendingDelete.key, {
      onSuccess: () => {
        setPendingDelete(undefined);
      },
      onError: (error: unknown) => {
        setActionError(configFailureReason(error) ?? 'delete');
        setPendingDelete(undefined);
      },
    });
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.mcpServers.description',
          'MCP servers offered to every project as ready-made toolkits. Each client secret is sealed in the platform vault and is never returned by any endpoint.',
        )}
      </Typography>

      {listQuery.isLoading ? <LinearProgress /> : null}

      <McpCatalogueAlerts
        loadError={listQuery.error}
        actionError={actionError}
        onDismissAction={() => {
          setActionError(undefined);
        }}
      />

      <Box>
        <Button
          size="small"
          variant="elitea" color="primary"
          onClick={openCreate}
          sx={{ textTransform: 'none' }}
          data-testid="admin-mcp-servers-add"
        >
          {t('pages.admin.mcpServers.add', 'Add MCP server')}
        </Button>
      </Box>

      {!listQuery.isLoading && servers.length === 0 && listQuery.error == null ? (
        <Typography variant="bodyMedium" color="text.secondary" data-testid="admin-mcp-servers-empty">
          {t(
            'pages.admin.mcpServers.empty',
            'No MCP servers are catalogued. Projects can still configure their own remote MCP toolkits.',
          )}
        </Typography>
      ) : null}

      {servers.length > 0 ? (
        <McpServerTable servers={servers} onEdit={openEdit} onRemove={setPendingDelete} />
      ) : null}

      <AdminMcpServerDialog
        open={dialogOpen}
        editing={editing}
        existingKeys={existingKeys}
        isSaving={saveMutation.isPending}
        serverError={
          saveError === 'save'
            ? t('pages.admin.mcpServers.error.save', 'Failed to save that MCP server.')
            : saveError
        }
        onClose={() => {
          setDialogOpen(false);
          setEditing(undefined);
        }}
        onSubmit={handleSubmit}
      />

      <Dialog
        open={pendingDelete !== undefined}
        onClose={() => {
          setPendingDelete(undefined);
        }}
        maxWidth="xs"
        fullWidth
        data-testid="admin-mcp-server-delete-dialog"
      >
        <DialogTitle>{t('pages.admin.mcpServers.delete.title', 'Remove MCP server')}</DialogTitle>
        <DialogContent>
          <Typography variant="bodyMedium">
            {deleteWarning(pendingDelete?.display_name ?? '')}
          </Typography>
        </DialogContent>
        <DialogActions>
          <Button
            variant="elitea" color="tertiary"
            onClick={() => {
              setPendingDelete(undefined);
            }}
            disabled={deleteMutation.isPending}
            sx={{ textTransform: 'none' }}
          >
            {t('pages.admin.mcpServers.delete.cancel', 'Cancel')}
          </Button>
          <Button
            variant="elitea" color="alarm"
            onClick={handleDelete}
            disabled={deleteMutation.isPending}
            sx={{ textTransform: 'none' }}
            data-testid="admin-mcp-server-delete-confirm"
          >
            {t('pages.admin.mcpServers.delete.confirm', 'Remove')}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}
