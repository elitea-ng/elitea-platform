/**
 * Admin › Delegations awaiting review (ADR-0044 Amendment 1, A1.3; epic 2156
 * story S6): the queue per project with the calling agents to open, what the
 * migration did and did not do, what the mode means during it, and the scan.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import type { DelegationReview } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminDelegationReview } from './DelegationReview';
import { ConfigurationDelegationStatus } from './ConfigurationDelegationStatus';
import { renderAdminRoute } from './__tests__/testRouter';
import { enabledWriteControls, findViewOnlyNotice, serveConsoleLevel } from './__tests__/viewOnly';

const REVIEW: DelegationReview = {
  pending: 3,
  legacy: 2,
  stale: 1,
  mode: 'enforce',
  mode_stored: false,
  principal_bound: true,
  report_window_days: 14,
  projects: [
    {
      project_id: 7,
      project_name: 'Support',
      pending: 3,
      legacy: 2,
      stale: 1,
      callers: [{ application_id: 31, name: 'Triage agent', pending: 3, stale: 1 }],
    },
  ],
};

let review: DelegationReview;
let scans: number;

beforeEach(() => {
  review = REVIEW;
  scans = 0;
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/delegation_review', () => HttpResponse.json(review)),
    http.post('*/admin/delegation_review/scan', () => {
      scans += 1;
      review = { ...REVIEW, pending: 4, legacy: 3 };
      return HttpResponse.json({ projects: 1, scanned: 1, failed: 0, edges: 5, created: 1, notified: 2 });
    }),
  );
});

afterEach(() => resetGeneratedClient());

describe('AdminDelegationReview', () => {
  it('counts what awaits review per project and links each calling agent', async () => {
    renderAdminRoute(<AdminDelegationReview />);
    const row = await screen.findByTestId('delegation-review-project-7');
    expect(row).toHaveTextContent('Support');
    expect(screen.getByTestId('delegation-review-pending-7')).toHaveTextContent('3');
    expect(screen.getByTestId('delegation-review-count')).toHaveTextContent(
      '3 delegations await a first approval (2 from the migration of existing compositions) and 1 need re-approval, in 1 projects.',
    );
    expect(screen.getByTestId('delegation-review-count')).toHaveTextContent('Nothing was approved by the migration');
    expect(screen.getByTestId('delegation-review-open-7-31')).toHaveAttribute('href', '/app/7/agents/all/31');
  });

  it('says that an installation with no saved mode enforces and how to use the report window', async () => {
    renderAdminRoute(<AdminDelegationReview />);
    const mode = await screen.findByTestId('delegation-review-mode');
    expect(mode).toHaveTextContent('No mode was ever saved, so delegation enforces');
    expect(mode).toHaveTextContent('about 14 days');
  });

  it('says that report is a window and never relaxes the principal bound', async () => {
    review = { ...REVIEW, mode: 'report', mode_stored: true };
    renderAdminRoute(<AdminDelegationReview />);
    const mode = await screen.findByTestId('delegation-review-mode');
    expect(mode).toHaveTextContent('The mode is report');
    expect(mode).toHaveTextContent('Report never relaxes the run principal bound');
  });

  it('shows an empty queue as done', async () => {
    review = { ...REVIEW, pending: 0, legacy: 0, stale: 0, projects: [] };
    renderAdminRoute(<AdminDelegationReview />);
    expect(await screen.findByTestId('delegation-review-clear')).toBeInTheDocument();
    expect(screen.getByText('No project has a delegation to review.')).toBeInTheDocument();
  });

  it('scans the compositions again and refreshes the queue', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminDelegationReview />);
    await screen.findByTestId('delegation-review-project-7');
    await user.click(screen.getByTestId('delegation-review-scan'));
    expect(await screen.findByTestId('delegation-review-scanned')).toHaveTextContent(
      'Scanned 5 compositions; 1 pending grants were written.',
    );
    expect(scans).toBe(1);
    await waitFor(() => expect(screen.getByTestId('delegation-review-count')).toHaveTextContent('4 delegations await a first approval (3 from'));
  });

  it('shows a failed read as an error, never as an empty queue', async () => {
    server.use(http.get('*/admin/delegation_review', () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    renderAdminRoute(<AdminDelegationReview />);
    expect(await screen.findByText('Could not read the delegations awaiting review.')).toBeInTheDocument();
    expect(screen.queryByTestId('delegation-review-clear')).not.toBeInTheDocument();
  });
});

describe('ConfigurationDelegationStatus', () => {
  it('shows the pending count, the way to the queue and the report window next to the Delegation settings', () => {
    render(<ConfigurationDelegationStatus status={{ pending: 3, legacy: 2, stale: 1, report_window_days: 14 }} />);
    expect(screen.getByTestId('admin-delegation-pending')).toHaveTextContent('4 delegations await review (2 from the migration');
    expect(screen.getByTestId('admin-delegation-review-link')).toHaveAttribute('href', '/admin/app/delegation-review');
    expect(screen.getByTestId('admin-delegation-window')).toHaveTextContent('about 14 days');
  });

  it('says when nothing awaits review and renders nothing without a status', () => {
    const { rerender } = render(<ConfigurationDelegationStatus status={{ pending: 0, stale: 0 }} />);
    expect(screen.getByTestId('admin-delegation-pending')).toHaveTextContent('No delegation awaits review.');
    rerender(<ConfigurationDelegationStatus status={undefined} />);
    expect(screen.queryByTestId('admin-delegation-status')).not.toBeInTheDocument();
  });
});

// Read-only mode (R3.4b, issue 2613): at the read level of `delegation-review` the
// notice shows and every write is disabled, not hidden.
describe('view only', () => {
  it('disables the writes at the read level and keeps the listing', async () => {
    serveConsoleLevel('read', 'delegation-review');
    renderAdminRoute(<AdminDelegationReview />);
    await screen.findByTestId('delegation-review-project-7');
    await findViewOnlyNotice();
    await waitFor(() => expect(screen.getByTestId('delegation-review-scan')).toBeDisabled());
    expect(enabledWriteControls()).toEqual([]);
  });
});
