/**
 * Admin › Delegations awaiting review (ADR-0044 Amendment 1, A1.3; delegation
 * epic 2156 story S6). When an agent attaches another agent that holds more than
 * it does, the edge needs a delegation grant that an editor of the caller
 * approves. The migration of existing compositions wrote a PENDING grant for each
 * such edge that existed before the upgrade and approved none, so nothing was
 * widened; this page is the administrator's view of what is left to review, per
 * project, with the calling agents to open.
 *
 * It also says what the mode means during the migration: an installation with no
 * saved mode enforces, which already refuses these edges; `report` is a window
 * (recommended 14 days) in which the delegated agent runs and the missing grant is
 * recorded, and it never relaxes the run principal bound. Behind
 * `configuration.governance`, like the Delegation section it explains.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import LinearProgress from '@mui/material/LinearProgress';
import Link from '@mui/material/Link';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { DelegationReview, DelegationReviewProject } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminViewOnlyNotice, AdminWritable } from './AdminWritable';
import { ADMIN_ITEM } from './adminItemIds';
import { useAdminWritable } from './adminWriteAccess';
import { useDelegationReview, useScanDelegations } from './api/adminDelegationReviewApi';

/** The calling agent's page in the main app: the Tools panel carries the banner and the review. */
function agentHref(projectId: number, applicationId: number): string {
  return `/app/${projectId}/agents/all/${applicationId}`;
}

function modeSentence(review: DelegationReview): string {
  if (review.mode === 'report') {
    return t(
      'pages.admin.delegationReview.modeReport',
      'The mode is report: a delegation without an approved grant runs and is recorded. Return to enforce when the owners have reviewed, about {{days}} days after the upgrade. Report never relaxes the run principal bound.',
      { days: review.report_window_days },
    );
  }
  return review.mode_stored
    ? t(
        'pages.admin.delegationReview.modeEnforce',
        'The mode is enforce: a delegation without an approved grant is refused, and the chat shows the refusal with Review delegation. To keep these calls running while owners review, set the mode to report for a window of about {{days}} days on the Delegation settings, then return to enforce.',
        { days: review.report_window_days },
      )
    : t(
        'pages.admin.delegationReview.modeEnforceDefault',
        'No mode was ever saved, so delegation enforces: a delegation without an approved grant is refused, and the chat shows the refusal with Review delegation. To keep these calls running while owners review, save the mode as report for a window of about {{days}} days on the Delegation settings, then return to enforce.',
        { days: review.report_window_days },
      );
}

function ProjectRow({ project }: { readonly project: DelegationReviewProject }): ReactNode {
  return (
    <TableRow data-testid={`delegation-review-project-${project.project_id}`} sx={rowSx}>
      <TableCell>
        <Typography variant="bodyMedium" sx={wrapSx}>
          {project.project_name === '' ? `#${project.project_id}` : project.project_name}
        </Typography>
        <Typography variant="bodySmall" color="text.secondary">
          #{project.project_id}
        </Typography>
      </TableCell>
      <TableCell data-testid={`delegation-review-pending-${project.project_id}`}>{project.pending}</TableCell>
      <TableCell>{project.legacy}</TableCell>
      <TableCell>{project.stale}</TableCell>
      <TableCell>
        <Box component="ul" sx={listSx}>
          {project.callers.map((caller) => (
            <li key={caller.application_id}>
              <Link
                href={agentHref(project.project_id, caller.application_id)}
                underline="hover"
                data-testid={`delegation-review-open-${project.project_id}-${caller.application_id}`}
              >
                {caller.name === '' ? `#${caller.application_id}` : caller.name}
              </Link>{' '}
              <Typography component="span" variant="bodySmall" color="text.secondary">
                {t('pages.admin.delegationReview.callerCounts', '{{pending}} to approve, {{stale}} to re-approve', {
                  pending: caller.pending,
                  stale: caller.stale,
                })}
              </Typography>
            </li>
          ))}
        </Box>
      </TableCell>
    </TableRow>
  );
}

function Summary({ review }: { readonly review: DelegationReview }): ReactNode {
  return (
    <Box sx={summarySx} data-testid="delegation-review-summary">
      {review.pending + review.stale === 0 ? (
        <Alert severity="success" data-testid="delegation-review-clear">
          {t('pages.admin.delegationReview.clear', 'No delegation awaits review. Every attached agent that holds more than its caller has an approved grant.')}
        </Alert>
      ) : (
        <Alert severity="warning" data-testid="delegation-review-count">
          {t(
            'pages.admin.delegationReview.count',
            '{{pending}} delegations await a first approval ({{legacy}} from the migration of existing compositions) and {{stale}} need re-approval, in {{projects}} projects. Nothing was approved by the migration, and no agent can use more than it could before.',
            { pending: review.pending, legacy: review.legacy, stale: review.stale, projects: review.projects.length },
          )}
        </Alert>
      )}
      <Alert severity="info" data-testid="delegation-review-mode">
        {modeSentence(review)}
      </Alert>
    </Box>
  );
}

export function AdminDelegationReview(): ReactNode {
  return (
    <AdminWritable itemId={ADMIN_ITEM.delegationReview}>
      <DelegationReviewPage />
    </AdminWritable>
  );
}

function DelegationReviewPage(): ReactNode {
  const review = useDelegationReview();
  const access = useAdminWritable(ADMIN_ITEM.delegationReview);
  const scan = access.guard(useScanDelegations());

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem' }}>
      <Typography variant="headingLarge" component="h1" sx={titleSx}>
        {t('pages.admin.delegationReview.title', 'Delegations awaiting review')}
      </Typography>
      <Typography variant="bodyMedium" color="text.secondary" sx={subtitleSx}>
        {t(
          'pages.admin.delegationReview.subtitle',
          'An agent that attaches another agent holding more than it does needs a delegation grant, approved by an editor of the calling agent. Open an agent to review its delegations.',
        )}
      </Typography>

      <AdminViewOnlyNotice />

      {review.isFetching && <LinearProgress data-testid="delegation-review-loading" />}
      {review.isError && (
        <Alert severity="error">{t('pages.admin.delegationReview.loadError', 'Could not read the delegations awaiting review.')}</Alert>
      )}
      {review.data !== undefined && <Summary review={review.data} />}

      <Box sx={actionsSx}>
        <Button
          size="small"
          variant="outlined"
          disabled={!access.writable || scan.isPending}
          onClick={() => scan.mutate()}
          data-testid="delegation-review-scan"
        >
          {t('pages.admin.delegationReview.scan', 'Scan compositions again')}
        </Button>
        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'pages.admin.delegationReview.scanHint',
            'Writes a pending grant for any attached agent that holds more than its caller and has none. It approves nothing and is safe to repeat.',
          )}
        </Typography>
      </Box>
      {scan.isSuccess && (
        <Alert severity="success" onClose={() => scan.reset()} data-testid="delegation-review-scanned">
          {t('pages.admin.delegationReview.scanned', 'Scanned {{edges}} compositions; {{created}} pending grants were written.', {
            edges: scan.data.edges,
            created: scan.data.created,
          })}
        </Alert>
      )}
      {scan.isError && (
        <Alert severity="error">{t('pages.admin.delegationReview.scanError', 'The scan did not finish. Try again.')}</Alert>
      )}

      <Table size="small" data-testid="delegation-review-table">
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.delegationReview.column.project', 'Project')}</TableCell>
            <TableCell>{t('pages.admin.delegationReview.column.pending', 'Awaiting approval')}</TableCell>
            <TableCell>{t('pages.admin.delegationReview.column.legacy', 'From the migration')}</TableCell>
            <TableCell>{t('pages.admin.delegationReview.column.stale', 'Need re-approval')}</TableCell>
            <TableCell>{t('pages.admin.delegationReview.column.agents', 'Calling agents to open')}</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {review.data?.projects.length === 0 && (
            <TableRow>
              <TableCell colSpan={5}>{t('pages.admin.delegationReview.empty', 'No project has a delegation to review.')}</TableCell>
            </TableRow>
          )}
          {review.data?.projects.map((project) => <ProjectRow key={project.project_id} project={project} />)}
        </TableBody>
      </Table>
    </DrawerPage>
  );
}

const titleSx: SxProps<Theme> = { marginBottom: '0.25rem' };
const subtitleSx: SxProps<Theme> = { marginBottom: '1rem' };
const summarySx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem', marginBottom: '1rem' };
const actionsSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '0.75rem', marginBottom: '1rem' };
const rowSx: SxProps<Theme> = { verticalAlign: 'top' };
const wrapSx: SxProps<Theme> = { overflowWrap: 'anywhere' };
const listSx: SxProps<Theme> = { margin: 0, paddingLeft: '1.25rem' };
