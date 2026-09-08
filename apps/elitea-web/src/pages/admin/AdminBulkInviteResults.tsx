/**
 * The per-pair result table of the bulk invite (issue 247).
 *
 * Split out of `./AdminBulkInviteDialog` because the outcome vocabulary is a
 * fact about the SERVER's contract, not about the dialog's form state, and
 * because eight branches in one render put that component past the repo's
 * complexity budget.
 *
 * Every outcome gets its own sentence. pylon joined English prose into one
 * textarea, so an operator could not tell "added" from "was already a member"
 * from "that project has no such role" without reading every line — and the
 * three call for three different next actions, or for none.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';

import { t } from '@/shared/i18n';

import type { BulkInviteOutcome, BulkInviteResultRow } from './api/adminBulkInviteApi';

export interface BulkInviteResultsProps {
  readonly results: readonly BulkInviteResultRow[];
}

/**
 * The chip colour per outcome. `already_member` is NOT a warning: the operator
 * asked for the account to be in the project, and it is.
 */
function outcomeColour(outcome: BulkInviteOutcome): 'success' | 'default' | 'warning' | 'error' {
  if (outcome === 'added') return 'success';
  if (outcome === 'already_member') return 'default';
  if (outcome === 'failed') return 'error';
  return 'warning';
}

function outcomeLabel(outcome: BulkInviteOutcome): string {
  const labels: Record<BulkInviteOutcome, string> = {
    added: t('pages.admin.users.bulkInvite.outcome.added', 'Added'),
    already_member: t('pages.admin.users.bulkInvite.outcome.alreadyMember', 'Already a member'),
    unknown_user: t('pages.admin.users.bulkInvite.outcome.unknownUser', 'No such user'),
    unknown_project: t('pages.admin.users.bulkInvite.outcome.unknownProject', 'No such project'),
    unknown_role: t('pages.admin.users.bulkInvite.outcome.unknownRole', 'Role not defined here'),
    system_user: t('pages.admin.users.bulkInvite.outcome.systemUser', 'Service account'),
    personal_project: t('pages.admin.users.bulkInvite.outcome.personalProject', 'Personal project'),
    failed: t('pages.admin.users.bulkInvite.outcome.failed', 'Failed'),
  };
  return labels[outcome];
}

export function BulkInviteResults({ results }: BulkInviteResultsProps): ReactNode {
  if (results.length === 0) return null;
  return (
    <Box sx={{ maxHeight: '18rem', overflowY: 'auto' }} data-testid="bulk-invite-results">
      <Table size="small" stickyHeader>
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.users.bulkInvite.column.user', 'User')}</TableCell>
            <TableCell>{t('pages.admin.users.bulkInvite.column.project', 'Project')}</TableCell>
            <TableCell>{t('pages.admin.users.bulkInvite.column.outcome', 'Outcome')}</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {results.map((result) => (
            <TableRow key={`${result.user_id}:${result.project_id}`}>
              {/* The id is the fallback, not a blank: a pair whose account or
                  project could not be resolved carries an empty name, and an
                  empty cell would read as a rendering fault rather than as the
                  refusal the chip beside it states.
                  `id 42`, not `#42`: R-T1's raw-colour rule reads a `#` in
                  front of digits as a hex literal (theme-gate), and the word
                  is clearer to an operator anyway. */}
              <TableCell>
                {result.user_email === '' ? `id ${result.user_id}` : result.user_email}
              </TableCell>
              <TableCell>
                {result.project_name === '' ? `id ${result.project_id}` : result.project_name}
              </TableCell>
              <TableCell>
                <Chip
                  size="small"
                  color={outcomeColour(result.outcome)}
                  label={outcomeLabel(result.outcome)}
                  title={result.msg}
                />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Box>
  );
}
