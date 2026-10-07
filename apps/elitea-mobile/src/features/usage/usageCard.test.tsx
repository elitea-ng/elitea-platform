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
  expect(screen.getByText('Usage this month')).toBeTruthy();
  expect(screen.getByLabelText('You: No limit')).toBeTruthy();
  expect(screen.getByLabelText('Project: No limit')).toBeTruthy();
  expect(screen.getByText(/2026-10-01 to 2026-10-31/)).toBeTruthy();
});

it('is hidden when the workspace answers neither scope', async () => {
  mockData = {};
  await render(<UsageCard projectId={90504} />);
  expect(screen.queryByTestId('usage-card')).toBeNull();
});
