/**
 * Admin › Users › "Bulk invite" — many accounts into many projects with one
 * role (issue 247).
 *
 * ## What it replaces
 *
 * Two pylon console pages, each a textarea over the same write:
 * `legacy/plugins/admin/api/v2/invites_bulkusers.py` (with
 * `static/js/invites_bulkusers.js`: a project-id box, a comma-separated roles
 * box, "Add all users", and a read-only log pane) and `invites_bulkprojects.py`
 * (a user-id box, the same roles box, and a checkbox table of projects).
 *
 * This is one dialog because both pages are the same cross product with one
 * side pinned. Three things are deliberately different:
 *
 *  1. **Pickers, not id boxes.** The reference asks an operator to type a
 *     numeric project id and a numeric user id. Both lists are already served
 *     to this page, so both are multi-selects over real rows.
 *  2. **One typed role, not a comma-separated string.** The server resolves the
 *     name per project against `auth_core__project_role` and refuses a name the
 *     project does not define, so a free-text box can only produce a refusal an
 *     operator cannot predict.
 *  3. **The result is a table, not a log blob.** pylon joins English sentences
 *     with newlines into a textarea, so "added" and "was already a member" and
 *     "that project has no such role" are indistinguishable without reading
 *     every line. The server answers a machine-readable `outcome` per pair.
 *
 * ## A resolved write is not a successful write
 *
 * The route answers **200 even when pairs failed** — the batch is a report, and
 * a 400 would say nothing landed when something did. So this dialog branches on
 * `report.failed`, never on the promise resolving. Reporting success from a
 * resolved promise is precisely the defect `ProjectMemberDialog`'s header
 * records in the reference client.
 *
 * ## Authorisation
 *
 * The route is gated server-side on `invites.bulkusers` / `invites.bulkprojects`
 * in administration mode, resolved from the database on every request. The
 * admin-panel config flag below is PRESENTATION state and never a gate —
 * see `./adminUiConfig`.
 */
import { useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Autocomplete from '@mui/material/Autocomplete';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import MenuItem from '@mui/material/MenuItem';
import Stack from '@mui/material/Stack';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { BulkInviteResults } from './AdminBulkInviteResults';
import { useAdminUsers, type AdminUserRow } from './api/adminUsersApi';
import { useAdminProjects, useProjectRoles, type AdminProjectRow } from './api/adminProjectsApi';
import { useBulkInviteMembers, type BulkInviteReport } from './api/adminBulkInviteApi';

export interface AdminBulkInviteDialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
}

/**
 * How many rows each picker offers.
 *
 * It is a WINDOW, not the whole platform, and the dialog says so: a deployment
 * with fifteen thousand accounts cannot render them all, and a picker that
 * silently showed the first two hundred would let an operator believe an
 * account does not exist. The search box narrows the same window server-side.
 */
const PICKER_PAGE_SIZE = 200;

/** The cross-product cap the server enforces (`maxBulkInvitePairs`). */
const MAX_PAIRS = 2000;

function userLabel(user: AdminUserRow): string {
  return user.name === '' ? user.email : `${user.name} (${user.email})`;
}

/**
 * The role options.
 *
 * They come from the FIRST selected project, because role names are per project
 * (`auth_core__project_role` is keyed on `(project_id, name)`) and there is no
 * platform-wide list to read. Every project this platform provisions gets the
 * same four names, so in practice the list is the same everywhere; where it is
 * not, the server reports `unknown_role` for that pair and the results table
 * names the project. That is a better answer than hiding a role the other
 * projects do define.
 */
function useRoleOptions(projects: readonly AdminProjectRow[]): {
  readonly names: readonly string[];
  readonly isLoading: boolean;
} {
  const firstProjectId = projects[0]?.id ?? 0;
  const rolesQuery = useProjectRoles(firstProjectId);
  return {
    names: (rolesQuery.data ?? []).map((role) => role.name),
    isLoading: firstProjectId !== 0 && rolesQuery.isFetching,
  };
}

/** The summary line, written once so the banner and its test cannot drift. */
function summaryText(report: BulkInviteReport): string {
  return t(
    'pages.admin.users.bulkInvite.summary',
    '{{added}} added, {{skipped}} already members, {{failed}} refused of {{requested}} pairs.',
    {
      added: report.added,
      skipped: report.skipped,
      failed: report.failed,
      requested: report.requested,
    },
  );
}

/**
 * The form's three notices. Extracted for the reason `ProjectMemberDialog`'s
 * `MemberAlerts` is: three independent conditional alerts are three branches,
 * none of which is about the form's behaviour, and together they push the form
 * past the repo's complexity budget (12).
 */
function BulkInviteAlerts({
  errorMessage,
  report,
  pairCount,
  onDismissError,
}: {
  readonly errorMessage: string;
  readonly report: BulkInviteReport | null;
  readonly pairCount: number;
  readonly onDismissError: () => void;
}) {
  return (
    <>
      {errorMessage !== '' ? (
        <Alert severity="error" onClose={onDismissError} data-testid="bulk-invite-error">
          {errorMessage}
        </Alert>
      ) : null}
      {report !== null ? (
        <Alert severity={report.failed > 0 ? 'warning' : 'success'} data-testid="bulk-invite-summary">
          {summaryText(report)}
        </Alert>
      ) : null}
      {pairCount > MAX_PAIRS ? (
        <Alert severity="error" data-testid="bulk-invite-over-cap">
          {t(
            'pages.admin.users.bulkInvite.overCap',
            '{{pairs}} pairs exceeds the limit of {{cap}}. Select fewer users or fewer projects.',
            { pairs: pairCount, cap: MAX_PAIRS },
          )}
        </Alert>
      ) : null}
    </>
  );
}

export function AdminBulkInviteDialog({ open, onClose }: AdminBulkInviteDialogProps) {
  return (
    <Dialog open={open} onClose={onClose} maxWidth="md" fullWidth>
      <DialogTitle>{t('pages.admin.users.bulkInvite.title', 'Bulk invite')}</DialogTitle>
      {open ? <AdminBulkInviteForm onClose={onClose} /> : null}
    </Dialog>
  );
}

/**
 * The form holds every piece of state, and the shell above mounts it only while
 * the dialog is open. Closing therefore discards the selection and the previous
 * report, so a second batch cannot inherit the first one's results table — the
 * leak `ProjectMemberDialog` keys its own form against.
 */
function AdminBulkInviteForm({ onClose }: { readonly onClose: () => void }) {
  const [userSearch, setUserSearch] = useState('');
  const [projectSearch, setProjectSearch] = useState('');
  const [selectedUsers, setSelectedUsers] = useState<readonly AdminUserRow[]>([]);
  const [selectedProjects, setSelectedProjects] = useState<readonly AdminProjectRow[]>([]);
  const [role, setRole] = useState('');
  const [report, setReport] = useState<BulkInviteReport | null>(null);
  const [errorMessage, setErrorMessage] = useState('');

  const usersQuery = useAdminUsers({
    limit: PICKER_PAGE_SIZE,
    offset: 0,
    search: userSearch === '' ? undefined : userSearch,
    userType: 'platform',
  });
  const projectsQuery = useAdminProjects({
    limit: PICKER_PAGE_SIZE,
    offset: 0,
    search: projectSearch === '' ? undefined : projectSearch,
    projectType: 'team',
  });
  const roleOptions = useRoleOptions(selectedProjects);
  const invite = useBulkInviteMembers();

  const pairCount = selectedUsers.length * selectedProjects.length;
  const overCap = pairCount > MAX_PAIRS;
  const canSubmit = pairCount > 0 && role !== '' && !overCap && !invite.isPending;

  /* The picker rows. `useMemo` keeps the Autocomplete's option identity stable
     across the re-renders a selection causes; a fresh array each render makes
     MUI re-open the list on every keystroke. */
  const userOptions = useMemo(() => usersQuery.data?.rows ?? [], [usersQuery.data]);
  const projectOptions = useMemo(() => projectsQuery.data?.rows ?? [], [projectsQuery.data]);

  const handleSubmit = () => {
    if (!canSubmit) return;
    setReport(null);
    setErrorMessage('');
    invite.mutate(
      {
        users: selectedUsers.map((user) => user.id),
        projects: selectedProjects.map((project) => project.id),
        role,
      },
      {
        onSuccess: (result) => setReport(result),
        onError: (error: unknown) =>
          setErrorMessage(
            error instanceof Error && error.message
              ? error.message
              : t('pages.admin.users.bulkInvite.error', 'The bulk invite failed.'),
          ),
      },
    );
  };

  return (
    <>
      <DialogContent>
        <Stack spacing={2} sx={{ paddingTop: '0.5rem' }}>
          <Typography variant="bodyMedium" color="text.secondary">
            {t(
              'pages.admin.users.bulkInvite.subtitle',
              'Every selected account is added to every selected project with the role below. An account that already holds the role is left alone.',
            )}
          </Typography>

          <BulkInviteAlerts
            errorMessage={errorMessage}
            report={report}
            pairCount={pairCount}
            onDismissError={() => setErrorMessage('')}
          />

          <Autocomplete
            multiple
            disableCloseOnSelect
            options={userOptions}
            value={[...selectedUsers]}
            getOptionLabel={userLabel}
            isOptionEqualToValue={(option, value) => option.id === value.id}
            loading={usersQuery.isFetching}
            onInputChange={(_event, value) => setUserSearch(value)}
            onChange={(_event, value) => setSelectedUsers(value)}
            renderValue={(values, getItemProps) =>
              values.map((user, index) => (
                <Chip
                  {...getItemProps({ index })}
                  key={user.id}
                  size="small"
                  label={user.email}
                />
              ))
            }
            renderInput={(params) => (
              <TextField
                {...params}
                size="small"
                label={t('pages.admin.users.bulkInvite.users', 'Users')}
                placeholder={t('pages.admin.users.bulkInvite.usersPlaceholder', 'Search by name or email')}
                data-testid="bulk-invite-users"
              />
            )}
          />

          <Autocomplete
            multiple
            disableCloseOnSelect
            options={projectOptions}
            value={[...selectedProjects]}
            getOptionLabel={(project: AdminProjectRow) => project.name}
            isOptionEqualToValue={(option, value) => option.id === value.id}
            loading={projectsQuery.isFetching}
            onInputChange={(_event, value) => setProjectSearch(value)}
            onChange={(_event, value) => setSelectedProjects(value)}
            renderValue={(values, getItemProps) =>
              values.map((project, index) => (
                <Chip
                  {...getItemProps({ index })}
                  key={project.id}
                  size="small"
                  label={project.name}
                />
              ))
            }
            renderInput={(params) => (
              <TextField
                {...params}
                size="small"
                label={t('pages.admin.users.bulkInvite.projects', 'Projects')}
                placeholder={t('pages.admin.users.bulkInvite.projectsPlaceholder', 'Search by project name')}
                data-testid="bulk-invite-projects"
              />
            )}
          />

          <TextField
            select
            fullWidth
            size="small"
            label={t('pages.admin.users.bulkInvite.role', 'Role')}
            value={role}
            disabled={roleOptions.names.length === 0}
            onChange={(event) => setRole(event.target.value)}
            helperText={
              selectedProjects.length === 0
                ? t('pages.admin.users.bulkInvite.roleHint', 'Select a project to load its roles.')
                : ''
            }
            data-testid="bulk-invite-role"
          >
            {roleOptions.names.map((name) => (
              <MenuItem key={name} value={name}>
                {name}
              </MenuItem>
            ))}
          </TextField>

          {report !== null ? <BulkInviteResults results={report.results} /> : null}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={invite.isPending}>
          {t('pages.admin.users.bulkInvite.close', 'Close')}
        </Button>
        <Button
          variant="contained"
          onClick={handleSubmit}
          disabled={!canSubmit}
          data-testid="bulk-invite-submit"
        >
          {invite.isPending
            ? t('pages.admin.users.bulkInvite.inviting', 'Inviting…')
            : t('pages.admin.users.bulkInvite.invite', 'Invite {{pairs}} pairs', { pairs: pairCount })}
        </Button>
      </DialogActions>
    </>
  );
}
