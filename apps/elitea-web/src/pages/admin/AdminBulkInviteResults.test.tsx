/**
 * The bulk-invite result table (issue 247).
 *
 * Every outcome the server can report gets its own visible sentence, and the
 * eight are not interchangeable: pylon joined them into one log blob, so an
 * operator could not tell "added" from "was already a member" from "that
 * project has no such role" without reading every line — and the three call for
 * three different next actions, or for none.
 *
 * A missing case would render as `undefined` in the chip, which reads as a
 * rendering fault rather than as the outcome it is.
 */
import { describe, expect, it } from 'vitest';
import { render, screen, within } from '@testing-library/react';

import { ThemeProvider } from '@mui/material/styles';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { BulkInviteResults } from './AdminBulkInviteResults';
import type { BulkInviteOutcome, BulkInviteResultRow } from './api/adminBulkInviteApi';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/** The eight outcomes, with the label each one must render. */
const OUTCOMES: ReadonlyArray<readonly [BulkInviteOutcome, string]> = [
  ['added', 'Added'],
  ['already_member', 'Already a member'],
  ['unknown_user', 'No such user'],
  ['unknown_project', 'No such project'],
  ['unknown_role', 'Role not defined here'],
  ['system_user', 'Service account'],
  ['personal_project', 'Personal project'],
  ['failed', 'Failed'],
];

function row(outcome: BulkInviteOutcome, index: number): BulkInviteResultRow {
  return {
    user_id: index + 1,
    user_email: `person${index}@example.com`,
    project_id: index + 100,
    project_name: `project-${index}`,
    status: outcome === 'added' || outcome === 'already_member' ? 'ok' : 'error',
    outcome,
    msg: `${outcome} detail`,
  };
}

function renderResults(results: readonly BulkInviteResultRow[]) {
  return render(
    <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
      <BulkInviteResults results={results} />
    </ThemeProvider>,
  );
}

describe('BulkInviteResults', () => {
  it('renders a distinct label for every outcome the server can report', () => {
    renderResults(OUTCOMES.map(([outcome], index) => row(outcome, index)));

    const table = within(screen.getByTestId('bulk-invite-results'));
    for (const [, label] of OUTCOMES) {
      expect(table.getByText(label)).toBeInTheDocument();
    }
    // Eight distinct labels, so no two outcomes collapsed into one word.
    expect(new Set(OUTCOMES.map(([, label]) => label)).size).toBe(OUTCOMES.length);
  });

  it('falls back to the id when the pair named a user or project that does not exist', () => {
    renderResults([
      {
        user_id: 4242,
        user_email: '',
        project_id: 9191,
        project_name: '',
        status: 'error',
        outcome: 'unknown_user',
        msg: 'no platform user with id 4242',
      },
    ]);

    // A blank cell would read as a rendering fault rather than as the refusal
    // the chip beside it states.
    const table = within(screen.getByTestId('bulk-invite-results'));
    expect(table.getByText('id 4242')).toBeInTheDocument();
    expect(table.getByText('id 9191')).toBeInTheDocument();
  });

  it('renders nothing at all for an empty report, rather than an empty table', () => {
    renderResults([]);
    expect(screen.queryByTestId('bulk-invite-results')).not.toBeInTheDocument();
  });
});
