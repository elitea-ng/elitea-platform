import { render, screen } from '@testing-library/react-native';

import { usageProjectMember, usageUserOwner } from '@/api/__fixtures__/settings13';
import { usageOf, type UsageSummary } from '@/api/queries/usage';
import { UsageCard } from '@/features/usage/UsageCard';

let mockData: Record<string, UsageSummary | null> = {};
jest.mock('@/api/queries/usage', () => ({
  ...jest.requireActual('@/api/queries/usage'),
  useUsage: (_p: number, scope: string) => ({ data: mockData[scope] ?? null }),
}));

it('shows the caller and the project for the month', async () => {
  mockData = { user: usageOf(usageUserOwner), project: usageOf(usageProjectMember) };
  await render(<UsageCard projectId={90504} />);
  expect(screen.getByText(/^usage this month$/i)).toBeTruthy();
  expect(screen.getByLabelText('You: No limit')).toBeTruthy();
  expect(screen.getByLabelText('Project: No limit')).toBeTruthy();
  expect(screen.getByText(/2026-10-01 to 2026-10-31/)).toBeTruthy();
});

it('is hidden when the workspace answers neither scope', async () => {
  mockData = {};
  await render(<UsageCard projectId={90504} />);
  expect(screen.queryByTestId('usage-card')).toBeNull();
});

it('draws a meter with its value for screen readers, and a warning with a symbol and words', async () => {
  const { UsageView } = jest.requireActual('@/features/usage/UsageCard') as typeof import('@/features/usage/UsageCard');
  const base = {
    scope: 'user' as const,
    periodStart: '2026-10-01',
    periodEnd: '2026-10-31',
    resetsAt: '2026-11-01T00:00:00Z',
    canSeeAmounts: true,
    unlimited: false,
    spendAvailable: true,
    spend: 46,
    limit: 50,
    remaining: 4,
    percentUsed: 92,
    warning: true,
    totals: { tokens: 1200, requests: 3 },
  };
  await render(<UsageView you={base} project={null} />);
  const row = screen.getByLabelText('You: $46.00 of $50.00 (92%), close to the limit');
  expect(row.props.accessibilityValue).toEqual({ min: 0, max: 100, now: 92 });
  expect(screen.getByRole('alert')).toHaveTextContent('Close to the limit. Requests stop when a budget is used up.');
  expect(screen.getByText('1,200 tokens · 3 requests')).toBeTruthy();
});
