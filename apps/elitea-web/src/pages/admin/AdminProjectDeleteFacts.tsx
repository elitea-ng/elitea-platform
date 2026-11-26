/**
 * What a project delete destroys, shown inside the one dangerous-action dialog
 * (`AdminDangerousActionDialog`, ADR-0059 R3.8b) of the admin Projects page.
 *
 * Reference (read-only):
 * `frontends/admin_ui/frontend/src/pages/ProjectsPage/DeleteProjectDialog.jsx`
 * — "Are you sure?" and a red button. This confirmation is deliberately
 * heavier, and the reason is what the call actually does.
 *
 * ## What is being destroyed
 *
 * `DELETE /projects/project/administration/{id}` runs the provisioning pipeline
 * in reverse and ends with `DROP SCHEMA p_<id> CASCADE`. That takes every
 * prompt, agent, conversation, artifact, datasource and audit row belonging to
 * the tenant. There is no soft-delete behind it, no retention window and no
 * restore path in the product — recovery means a database backup, if the
 * deployment has one.
 *
 * So: the names are listed rather than counted, a personal project is called
 * out separately (deleting one strips a real person's own workspace, and the
 * listing's `is_personal` flag is the only thing that distinguishes it from a
 * team project by name), and the operator types the target's name.
 *
 * ## Partial failure is reported per project
 *
 * There is no batch route: the page deletes one project per request. So "three
 * of five were deleted" is a state that can happen, and it is the only honest
 * report when it does.
 */
import Alert from '@mui/material/Alert';
import AlertTitle from '@mui/material/AlertTitle';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { AdminProjectRow } from './api/adminProjectsApi';

/** One project's outcome from the per-project delete loop. */
export interface ProjectDeleteFailure {
  readonly projectId: number;
  readonly name: string;
  readonly message: string;
}

/**
 * The dialog's copy. The operator types the project's name, or the count of a
 * multi-delete ("3 projects").
 */
export function projectDeleteCopy(projects: readonly AdminProjectRow[], isDeleting: boolean) {
  const count = projects.length;
  return {
    title:
      count === 1
        ? t('pages.admin.projects.delete.titleOne', 'Delete project')
        : t('pages.admin.projects.delete.titleMany', 'Delete {{count}} projects', { count }),
    change: t(
      'pages.admin.projects.delete.warningBody',
      'Deleting a project drops its tenant schema and everything in it — prompts, agents, conversations, artifacts, datasources and audit history — and tears down its object storage, vault secrets and system account. There is no restore.',
    ),
    reach: { count, unit: 'projects' as const },
    target: count === 1 ? (projects[0]?.name ?? '') : `${count} projects`,
    confirmLabel: isDeleting
      ? t('pages.admin.projects.delete.submitting', 'Deleting…')
      : t('pages.admin.projects.delete.submit', 'Delete permanently'),
  };
}

export interface AdminProjectDeleteFactsProps {
  /** The selected projects, in listing order. */
  readonly projects: readonly AdminProjectRow[];
  /** Projects the last attempt could not delete. Empty after a clean run. */
  readonly failures: readonly ProjectDeleteFailure[];
}

export function AdminProjectDeleteFacts({ projects, failures }: AdminProjectDeleteFactsProps) {
  const personal = projects.filter((project) => project.is_personal);
  return (
    <Box>
      <Typography variant="bodyMedium" sx={{ display: 'block', marginBottom: '0.5rem' }}>
        {t('pages.admin.projects.delete.listLabel', 'The following will be destroyed:')}
      </Typography>
      <Box
        component="ul"
        data-testid="admin-projects-delete-list"
        sx={{ margin: 0, marginBottom: '1rem', paddingLeft: '1.25rem' }}
      >
        {projects.map((project) => (
          <Typography key={project.id} component="li" variant="bodyMedium">
            {`${project.name} (ID: ${project.id})`}
          </Typography>
        ))}
      </Box>

      {personal.length > 0 ? (
        <Alert severity="warning" sx={{ marginBottom: '1rem' }}>
          {t(
            'pages.admin.projects.delete.personalWarning',
            '{{count}} of these is a personal project — a single user’s own workspace, not a shared one. Deleting it removes that person’s private prompts and agents.',
            { count: personal.length },
          )}
        </Alert>
      ) : null}

      {failures.length > 0 ? (
        <Alert severity="error">
          <AlertTitle>
            {t('pages.admin.projects.delete.partialTitle', 'Some projects were not deleted')}
          </AlertTitle>
          <Box component="ul" sx={{ margin: 0, paddingLeft: '1.25rem' }}>
            {failures.map((failure) => (
              <Typography key={failure.projectId} component="li" variant="bodySmall">
                {`${failure.name}: ${failure.message}`}
              </Typography>
            ))}
          </Box>
        </Alert>
      ) : null}
    </Box>
  );
}
