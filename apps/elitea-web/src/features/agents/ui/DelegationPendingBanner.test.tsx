import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';

import type { DelegationGrant } from '@/shared/api/generated/model';

import { DelegationPendingBanner } from './DelegationPendingBanner';

function grant(overrides: Partial<DelegationGrant>): DelegationGrant {
  return {
    id: 1,
    caller_application_id: 9,
    callee_application_id: 31,
    edge_kind: 'agent_tool',
    callee_version_id: 41,
    state: 'pending',
    legacy: false,
    governed: false,
    origin: 'attach',
    digest: 'd',
    requested_digest: 'd',
    items: [],
    requested_items: [],
    omitted: [],
    created_at: '2026-11-18T00:00:00Z',
    updated_at: '2026-11-18T00:00:00Z',
    ...overrides,
  };
}

describe('DelegationPendingBanner', () => {
  it('counts the legacy delegations that await review and says nothing was approved for the editor', () => {
    render(<DelegationPendingBanner grants={[grant({ id: 1, legacy: true }), grant({ id: 2, legacy: true, callee_application_id: 32 })]} />);
    const banner = screen.getByTestId('delegation-pending-banner');
    expect(banner).toHaveTextContent('2 delegations from before this release await review');
    expect(banner).toHaveAttribute('data-legacy', '2');
  });

  it('counts a mix of pending grants without calling them all legacy', () => {
    render(<DelegationPendingBanner grants={[grant({ id: 1, legacy: true }), grant({ id: 2 })]} />);
    expect(screen.getByTestId('delegation-pending-banner')).toHaveTextContent('2 delegations await review.');
  });

  it('names the change request under the governed profile', () => {
    render(<DelegationPendingBanner grants={[grant({ governed: true, legacy: true })]} />);
    expect(screen.getByTestId('delegation-pending-banner')).toHaveTextContent('A different approver decides each one in its change request.');
  });

  it('is absent when nothing is pending: approved, stale and revoked grants are not awaiting a first approval', () => {
    render(<DelegationPendingBanner grants={[grant({ state: 'approved' }), grant({ id: 2, state: 'stale' }), grant({ id: 3, state: 'revoked' })]} />);
    expect(screen.queryByTestId('delegation-pending-banner')).not.toBeInTheDocument();
  });
});
