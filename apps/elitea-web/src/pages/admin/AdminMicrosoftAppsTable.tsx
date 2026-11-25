/**
 * The Microsoft app registry table (Admin › Configuration › Microsoft apps).
 *
 * One row per app: what it serves, which client it is, its state, whether a secret is
 * set (and how close it is to expiring), and which projects may use it. The client
 * secret is never shown, only that one is stored. A row that Helm seeds says so, so
 * an administrator knows a change here is overwritten at the next release only for
 * the values the chart sets.
 */
import Box from '@mui/material/Box';
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
import type { MicrosoftApp } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { typeLabel } from './AdminMicrosoftAppFields';
import { ADMIN_ACTION_PERMISSION } from './adminItemIds';
import { useAdminActionControl } from './adminWriteAccess';

export interface AdminMicrosoftAppsTableProps {
  readonly apps: readonly MicrosoftApp[];
  /** App ids whose write is in flight; their controls are disabled. */
  readonly busyIds: ReadonlySet<string>;
  readonly onEdit: (app: MicrosoftApp) => void;
  readonly onTest: (app: MicrosoftApp) => void;
  readonly onToggle: (app: MicrosoftApp) => void;
}

function stateLabel(app: MicrosoftApp): string {
  switch (app.state) {
    case 'active':
      return t('pages.admin.microsoftApps.state.active', 'Active');
    case 'draft':
      return t('pages.admin.microsoftApps.state.draft', 'Draft: no secret yet');
    case 'disabled':
      return t('pages.admin.microsoftApps.state.disabled', 'Disabled');
    default:
      return t('pages.admin.microsoftApps.state.needsAttention', 'Needs attention');
  }
}

function stateColor(state: string): 'success' | 'default' | 'warning' | 'error' {
  if (state === 'active') return 'success';
  if (state === 'needs_attention') return 'error';
  return state === 'draft' ? 'warning' : 'default';
}

function SecretCell({ app }: { readonly app: MicrosoftApp }) {
  if (!app.secret_set) {
    return <Typography variant="bodySmall">{t('pages.admin.microsoftApps.secret.none', 'Not set')}</Typography>;
  }
  const expiry = app.checklist.secret_expiry;
  return (
    <>
      <Typography variant="bodySmall">{t('pages.admin.microsoftApps.secret.set', 'Stored, hidden')}</Typography>
      {expiry === 'expired' || expiry === 'expiring' ? (
        <Chip
          size="small"
          color={expiry === 'expired' ? 'error' : 'warning'}
          label={
            expiry === 'expired'
              ? t('pages.admin.microsoftApps.secret.expired', 'Expired')
              : t('pages.admin.microsoftApps.secret.expiring', 'Expires in {{count}} days', {
                  count: Math.max(app.checklist.secret_days_left, 0),
                })
          }
        />
      ) : null}
    </>
  );
}

function ProjectsCell({ app }: { readonly app: MicrosoftApp }) {
  return (
    <Typography variant="bodySmall">
      {app.project_scope === 'selected'
        ? t('pages.admin.microsoftApps.projects.selected', '{{count}} projects', { count: app.project_ids.length })
        : t('pages.admin.microsoftApps.projects.all', 'Every project')}
    </Typography>
  );
}

export function AdminMicrosoftAppsTable({ apps, busyIds, onEdit, onTest, onToggle }: AdminMicrosoftAppsTableProps) {
  // View-only mode (R3.4b, issue 2613): every row control is disabled, not hidden.
  const viewOnly = useAdminActionControl(ADMIN_ACTION_PERMISSION.microsoftApps).disabled;
  return (
    <TableContainer>
      <Table size="small" aria-label={t('pages.admin.microsoftApps.tableLabel', 'Microsoft apps')} data-testid="admin-microsoft-apps-table">
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.microsoftApps.column.name', 'Name')}</TableCell>
            <TableCell>{t('pages.admin.microsoftApps.column.types', 'Serves')}</TableCell>
            <TableCell>{t('pages.admin.microsoftApps.column.state', 'State')}</TableCell>
            <TableCell>{t('pages.admin.microsoftApps.column.secret', 'Client secret')}</TableCell>
            <TableCell>{t('pages.admin.microsoftApps.column.projects', 'Projects')}</TableCell>
            <TableCell>{t('pages.admin.microsoftApps.column.enabled', 'Enabled')}</TableCell>
            <TableCell align="right">{t('pages.admin.microsoftApps.column.actions', 'Actions')}</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {apps.map((app) => {
            const busy = viewOnly || busyIds.has(app.id);
            return (
              <TableRow key={app.id} hover data-testid={`microsoft-app-row-${app.id}`}>
                <TableCell sx={{ minWidth: '10rem' }}>
                  <Typography variant="bodyMedium" component="div">
                    {app.name}
                  </Typography>
                  <Typography variant="bodySmall" component="div" color="text.secondary" sx={{ fontFamily: monoFontFamily, overflowWrap: 'anywhere' }}>
                    {app.client_id}
                  </Typography>
                  {app.managed_by === 'helm' ? (
                    <Chip size="small" variant="outlined" label={t('pages.admin.microsoftApps.managedByHelm', 'Seeded from Helm')} />
                  ) : null}
                </TableCell>
                <TableCell>
                  <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: '0.25rem' }}>
                    {app.credential_types.map((type) => (
                      <Chip key={type} size="small" variant="outlined" label={typeLabel(type)} />
                    ))}
                  </Box>
                </TableCell>
                <TableCell>
                  <Chip size="small" color={stateColor(app.state)} label={stateLabel(app)} />
                </TableCell>
                <TableCell>
                  <SecretCell app={app} />
                </TableCell>
                <TableCell>
                  <ProjectsCell app={app} />
                </TableCell>
                <TableCell>
                  <Switch
                    size="small"
                    checked={app.state !== 'disabled'}
                    disabled={busy}
                    onChange={() => {
                      onToggle(app);
                    }}
                    slotProps={{
                      input: { 'aria-label': t('pages.admin.microsoftApps.toggleLabel', 'Enable {{name}}', { name: app.name }) },
                    }}
                  />
                </TableCell>
                <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
                  <Button
                    variant="elitea"
                    color="tertiary"
                    size="small"
                    disabled={busy}
                    onClick={() => {
                      onTest(app);
                    }}
                    sx={{ textTransform: 'none' }}
                  >
                    {t('pages.admin.microsoftApps.test', 'Test')}
                  </Button>
                  <Button
                    variant="elitea"
                    color="tertiary"
                    size="small"
                    disabled={busy}
                    onClick={() => {
                      onEdit(app);
                    }}
                    sx={{ textTransform: 'none', ml: '0.25rem' }}
                  >
                    {t('pages.admin.microsoftApps.edit', 'Edit')}
                  </Button>
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </TableContainer>
  );
}
