/**
 * Admin › Projects — the deployment-wide project list (unit A14, issue #200).
 *
 * Reference (read-only): `frontends/admin_ui/frontend/src/pages/ProjectsPage/`
 * — 1884 lines across seven files, the largest page in the unit. Rewritten
 * against this app's stack: Redux Toolkit → TanStack Query
 * (`./api/adminProjectsApi`), axios → `eliteaFetch`, react-router → TanStack
 * Router (`./router`), MUI 7 → 9, and this app's `shared/ui` primitives instead
 * of admin_ui's own component set. State lives in `./useAdminProjectsPage`.
 *
 * ## What is real and what is not
 *
 * Of the reference's five mutations:
 *
 *  - **suspend / unsuspend** — real. The handler existed in elitea-main before
 *    this unit but was mounted on NO ROUTE; A14 routes and hardens it, gated on
 *    `projects.projects.projects.edit` and covered by write-then-re-read tests.
 *  - **add project admin** and **change a member's role** — real. Both reach
 *    `POST`/`PUT /admin/users/administration/{projectID}`, which answered 501
 *    before this unit because the handler treated `administration` as a
 *    project-less scope; the project id is in that path, so it is not.
 *  - **create project** and **delete project** — real, and the last two to
 *    become so. See below.
 *
 * The reference's export IS real here, and is the one place this port
 * deliberately differs in FORMAT: it writes CSV, not .xlsx, because this app
 * carries no spreadsheet dependency (see `./adminCsv`). The control says
 * "Export to CSV" so it never promises a file type it does not produce.
 *
 * Nothing here is a button that no-ops.
 *
 * ## Create and delete, and what changed
 *
 * Neither is one endpoint, and that is why both were withheld when this page
 * was first ported. `legacy/plugins/projects/utils/project_steps.py` runs NINE
 * steps to create a project — the row and its quota and statistics, the
 * `p_<id>` tenant schema, the permission set, a system user, that user's token,
 * the vault secrets, the object-storage buckets, the vector store — and
 * deletion runs the same steps in reverse, ending in
 * `DROP SCHEMA p_<id> CASCADE`. Half of that pipeline leaves orphaned
 * infrastructure around irreversibly destroyed tenant data.
 *
 * That pipeline now exists, ported in full:
 * `services/elitea-main/internal/application/projectprovisioning`. It runs the
 * ordered steps, records one status per step, and COMPENSATES every attempted
 * step in reverse when one fails — including the step that failed, because a
 * step can fail halfway through its own work. Two of the legacy nine are
 * deliberately not reproduced: the RabbitMQ vhost (AGENTS.md forbids the
 * Arbiter transport) and the InfluxDB databases. Both are drops with reasons
 * recorded in `steps.go`, not gaps.
 *
 * So the question the issue asked — what should deleting a project do about the
 * tenant schema and data? — is answered by the server, and this page's job is
 * to make the answer legible before the fact: `./AdminProjectDeleteDialog.tsx`
 * lists what is about to be destroyed, and reports which STEP failed when one
 * does. A provisioning failure is a position in a pipeline, not a status code.
 *
 * ## Authorisation
 *
 * `window.admin_ui_config.permissions` is presentation state and never a gate —
 * see `./adminUiConfig`. It does now carry the caller's REAL
 * administration-mode permissions (the handler injecting it used to write a
 * fixed 37-permission list for every session), which is what makes hiding the
 * create and delete controls per operator meaningful rather than decorative.
 * Meaningful, still not load-bearing. The listing is gated server-side on
 * `projects.projects.projects.view` and every write on its own permission,
 * resolved from `auth_core__user_role` per request. Projects are a tenancy
 * boundary: hiding a control here changes what an operator SEES, and a crafted
 * request is still refused by the server.
 */
import AddIcon from '@mui/icons-material/Add';
import DeleteIcon from '@mui/icons-material/Delete';
import FileDownloadOutlinedIcon from '@mui/icons-material/FileDownloadOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminProjectCreateDialog } from './AdminProjectCreateDialog';
import { AdminProjectDeleteDialog } from './AdminProjectDeleteDialog';
import { AdminProjectsTable } from './AdminProjectsTable';
import { ProjectActivityDrawer } from './ProjectActivityDrawer';
import { ProjectMemberDialog } from './ProjectMemberDialog';
import { ADMIN_PROJECTS_PAGE_SIZE, useAdminProjectsPage } from './useAdminProjectsPage';


export function AdminProjects() {
  const state = useAdminProjectsPage();

  const { total, page, provisioning } = state;
  const selectedCount = provisioning.selectedProjects.length;
  const lastPage = total === 0 ? 0 : Math.ceil(total / ADMIN_PROJECTS_PAGE_SIZE) - 1;
  const firstShown = total === 0 ? 0 : page * ADMIN_PROJECTS_PAGE_SIZE + 1;
  const lastShown = Math.min((page + 1) * ADMIN_PROJECTS_PAGE_SIZE, total);

  const createLabel = t('pages.admin.projects.action.create', 'Create project');
  const deleteLabel = t('pages.admin.projects.action.delete', 'Delete projects');
  const exportLabel = t('pages.admin.projects.action.export', 'Export to CSV');

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
          {t('pages.admin.projects.title', 'Projects')}
        </Typography>
        {/*
          The search box is the ONLY item allowed to shrink here. With every
          child shrinkable, a narrow viewport takes the width out of the
          buttons instead — their labels wrap to two lines inside a control
          whose height is a fixed 1.75rem (MuiButton.root), and the text spills
          out of the pill. The Users page shipped exactly that.
        */}
        <Box
          sx={{
            display: 'flex',
            alignItems: 'center',
            gap: '0.5rem',
            flex: '1 1 auto',
            justifyContent: 'flex-end',
            minWidth: 0,
          }}
        >
          <Box sx={{ flex: '1 1 12rem', minWidth: '8rem', maxWidth: '20rem' }}>
            <SimpleSearchBar
              value={state.search}
              onChange={state.onSearchChange}
              placeholder={t('pages.admin.projects.search', 'Search by name, ID or owner')}
              data-testid="admin-projects-search"
            />
          </Box>

          {/*
            Create and delete. Absent — not disabled — for an operator whose
            resolved permissions do not carry them, which is the convention the
            rest of this page already follows for the suspend and member
            controls: "this user may not" and "this deployment cannot" render
            identically, so the two can never disagree.

            Delete is additionally disabled while nothing is selected. That is a
            different kind of unavailable and it reads as one: the tooltip says
            what to do about it.
          */}
          {provisioning.onOpenCreate ? (
            <Button
              variant="elitea"
              color="primary"
              size="small"
              startIcon={<AddIcon fontSize="small" />}
              onClick={provisioning.onOpenCreate}
              // flexShrink/nowrap from #587: every child of this row used to be
              // shrinkable, so a narrow viewport took the width out of the
              // button and wrapped its label out of a fixed-height pill. The
              // search box is the only thing that may shrink.
              sx={{ flexShrink: 0, whiteSpace: 'nowrap' }}
            >
              {createLabel}
            </Button>
          ) : null}
          {provisioning.onOpenDelete ? (
            <Tooltip
              title={
                selectedCount === 0
                  ? t('pages.admin.projects.action.deleteHint', 'Select the projects to delete')
                  : deleteLabel
              }
            >
              <span>
                <IconButton
                  aria-label={deleteLabel}
                  color="error"
                  disabled={selectedCount === 0}
                  onClick={provisioning.onOpenDelete}
                  sx={{ flexShrink: 0 }}
                >
                  <DeleteIcon fontSize="small" />
                </IconButton>
              </span>
            </Tooltip>
          ) : null}

          <Tooltip title={exportLabel}>
            {/* `span`: a disabled button fires no events, so the tooltip needs
                a wrapper to hang its listeners on while the export runs. */}
            <span>
              <IconButton
                onClick={state.onExport}
                disabled={state.isExporting}
                aria-label={exportLabel}
                sx={{ flexShrink: 0 }}
              >
                {state.isExporting ? (
                  <CircularProgress size={16} />
                ) : (
                  <FileDownloadOutlinedIcon fontSize="small" />
                )}
              </IconButton>
            </span>
          </Tooltip>
        </Box>
      </Box>

      <Tabs value={state.activeTab} onChange={state.onTabChange} sx={{ minHeight: '2.5rem' }}>
        <Tab
          label={`${t('pages.admin.projects.tab.team', 'Team Projects')} (${state.counts.team})`}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
        <Tab
          label={`${t('pages.admin.projects.tab.personal', 'Personal Projects')} (${state.counts.personal})`}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
      </Tabs>

      {state.errorMessage ? (
        <Alert severity="error" onClose={state.onDismissError}>
          {state.errorMessage}
        </Alert>
      ) : null}

      {state.isError ? (
        <Alert severity="error">
          {t('pages.admin.projects.error.load', 'Failed to load projects.')}
        </Alert>
      ) : (
        <Box sx={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
          <AdminProjectsTable
            projects={state.rows}
            isLoading={state.isFetching}
            sortField={state.sortField}
            sortDirection={state.sortDirection}
            onSort={state.onSort}
            onToggleSuspended={state.onToggleSuspended}
            onOpenMembers={state.onOpenMembers}
            onOpenActivity={state.onOpenActivity}
            pendingIds={state.pendingIds}
            selectedIds={provisioning.selectedIds}
            onSelectionChange={provisioning.onSelectionChange}
          />

          <Box
            sx={{
              display: 'flex',
              justifyContent: 'flex-end',
              alignItems: 'center',
              gap: '0.75rem',
              paddingTop: '0.5rem',
            }}
          >
            <Typography variant="bodyMedium" color="text.secondary">
              {`${firstShown}–${lastShown} / ${total}`}
            </Typography>
            <Button
            variant="elitea" color="tertiary" size="small" disabled={page === 0} onClick={state.onPreviousPage}>
              {t('pages.admin.projects.pagination.previous', 'Previous')}
            </Button>
            <Button
            variant="elitea" color="tertiary" size="small" disabled={page >= lastPage} onClick={state.onNextPage}>
              {t('pages.admin.projects.pagination.next', 'Next')}
            </Button>
          </Box>
        </Box>
      )}

      <ProjectMemberDialog project={state.memberProject} onClose={state.onCloseMembers} />
      <ProjectActivityDrawer project={state.activityProject} onClose={state.onCloseActivity} />

      <AdminProjectCreateDialog
        open={provisioning.isCreateOpen}
        isSaving={provisioning.isCreating}
        serverError={provisioning.createError}
        failure={provisioning.createFailure}
        onClose={provisioning.onCloseCreate}
        onSubmit={provisioning.onCreate}
      />
      {/*
        Mounted only when something is selected. An empty confirmation dialog
        would be a dialog whose "Delete permanently" button destroys nothing —
        harmless, and exactly the kind of control that teaches an operator the
        button is safe to press.
      */}
      {selectedCount > 0 ? (
        <AdminProjectDeleteDialog
          open={provisioning.isDeleteOpen}
          projects={provisioning.selectedProjects}
          isDeleting={provisioning.isDeleting}
          failures={provisioning.deleteFailures}
          onClose={provisioning.onCloseDelete}
          onConfirm={provisioning.onConfirmDelete}
        />
      ) : null}
    </DrawerPage>
  );
}
