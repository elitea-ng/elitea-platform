/**
 * The native client registry table (Admin › Configuration › Native clients).
 *
 * A row from the `NATIVE_CLIENTS_PATH` FILE layer carries no controls: the file
 * is the deployment's own configuration, and nothing this page saves can change
 * or remove it. Saving a client with the same id would create a database row
 * that shadows it — a deliberate act the "Register" dialog still allows, but
 * not one an "Edit" button on the file row should perform behind the
 * operator's back. A database row that shadows a file entry says so, because
 * removing it brings the file entry back.
 */
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Switch from '@mui/material/Switch';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import { monoFontFamily } from '@/shared/brand/typeScale';
import type { NativeClient } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { isEditableNativeClient } from './adminNativeClientForm';

function SourceCell({ client }: { readonly client: NativeClient }) {
  if (!isEditableNativeClient(client)) {
    return (
      <>
        <Chip size="small" variant="outlined" label={t('pages.admin.nativeClients.source.file', 'File')} />
        <Typography variant="bodySmall" color="text.secondary" component="div">
          {t('pages.admin.nativeClients.source.fileNote', 'Read-only: set in the NATIVE_CLIENTS_PATH file.')}
        </Typography>
      </>
    );
  }
  return (
    <>
      <Chip size="small" variant="outlined" label={t('pages.admin.nativeClients.source.db', 'Database')} />
      {client.overridden_file ? (
        <Typography variant="bodySmall" color="text.secondary" component="div">
          {t('pages.admin.nativeClients.source.overrides', 'Overrides a file entry with the same ID.')}
        </Typography>
      ) : null}
    </>
  );
}

export interface NativeClientTableProps {
  readonly clients: readonly NativeClient[];
  /** Client ids whose write is in flight; their controls are disabled. */
  readonly busyIds: ReadonlySet<string>;
  readonly onEdit: (client: NativeClient) => void;
  readonly onToggleEnabled: (client: NativeClient) => void;
  readonly onRemove: (client: NativeClient) => void;
}

export function NativeClientTable({ clients, busyIds, onEdit, onToggleEnabled, onRemove }: NativeClientTableProps) {
  return (
    <TableContainer>
      <Table
        size="small"
        aria-label={t('pages.admin.nativeClients.tableLabel', 'Native clients')}
        data-testid="admin-native-clients-table"
      >
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.nativeClients.column.name', 'Name')}</TableCell>
            <TableCell>{t('pages.admin.nativeClients.column.redirectUris', 'Redirect URIs')}</TableCell>
            <TableCell>{t('pages.admin.nativeClients.column.minVersion', 'Minimum version')}</TableCell>
            <TableCell>{t('pages.admin.nativeClients.column.devices', 'Signed-in devices')}</TableCell>
            <TableCell>{t('pages.admin.nativeClients.column.source', 'Source')}</TableCell>
            <TableCell>{t('pages.admin.nativeClients.column.enabled', 'Enabled')}</TableCell>
            <TableCell align="right">{t('pages.admin.nativeClients.column.actions', 'Actions')}</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {clients.map((client) => {
            const editable = isEditableNativeClient(client);
            const busy = busyIds.has(client.client_id);
            const name = client.display_name !== '' ? client.display_name : client.client_id;
            return (
              <TableRow key={client.client_id} hover data-testid={`native-client-row-${client.client_id}`}>
                <TableCell sx={{ minWidth: '8rem' }}>
                  <Typography variant="bodyMedium" component="div">
                    {name}
                  </Typography>
                  <Typography
                    variant="bodySmall"
                    component="div"
                    color="text.secondary"
                    sx={{ fontFamily: monoFontFamily, overflowWrap: 'anywhere' }}
                  >
                    {client.client_id}
                  </Typography>
                </TableCell>
                {/* A floor width: without it the column shrank to a few
                    characters and broke every URI mid-word. */}
                <TableCell sx={{ minWidth: '12rem', overflowWrap: 'anywhere' }}>
                  {client.redirect_uris.map((uri) => (
                    <Typography key={uri} variant="bodySmall" component="div" sx={{ fontFamily: monoFontFamily }}>
                      {uri}
                    </Typography>
                  ))}
                </TableCell>
                <TableCell>
                  {client.min_client_version !== ''
                    ? client.min_client_version
                    : t('pages.admin.nativeClients.noMinVersion', 'None')}
                </TableCell>
                <TableCell data-testid={`native-client-devices-${client.client_id}`}>{client.active_devices}</TableCell>
                <TableCell>
                  <SourceCell client={client} />
                </TableCell>
                <TableCell>
                  <Switch
                    size="small"
                    checked={client.enabled}
                    disabled={!editable || busy}
                    onChange={() => {
                      onToggleEnabled(client);
                    }}
                    slotProps={{
                      input: {
                        'aria-label': t('pages.admin.nativeClients.toggleLabel', 'Enable {{name}}', { name }),
                      },
                    }}
                  />
                </TableCell>
                <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
                  {editable ? (
                    <>
                      <Button
                        variant="elitea"
                        color="tertiary"
                        size="small"
                        disabled={busy}
                        onClick={() => {
                          onEdit(client);
                        }}
                        sx={{ textTransform: 'none' }}
                      >
                        {t('pages.admin.nativeClients.edit', 'Edit')}
                      </Button>
                      <Button
                        variant="elitea"
                        color="alarm"
                        size="small"
                        disabled={busy}
                        onClick={() => {
                          onRemove(client);
                        }}
                        sx={{ textTransform: 'none', ml: '0.25rem' }}
                      >
                        {t('pages.admin.nativeClients.remove', 'Remove')}
                      </Button>
                    </>
                  ) : null}
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </TableContainer>
  );
}
