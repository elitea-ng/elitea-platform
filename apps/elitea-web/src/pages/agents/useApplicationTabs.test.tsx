import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { ApplicationsTabTotals } from './useApplicationsData';
import { privateApplicationTabValues, publicApplicationTabValues, useApplicationTabs } from './useApplicationTabs';

const TOTALS: ApplicationsTabTotals = {
  latestTotal: 3,
  myLikedTotal: undefined,
  trendingTotal: undefined,
  applicationsTotal: 5,
  draftTotal: undefined,
  publishedTotal: undefined,
  moderationTotal: undefined,
  approvalTotal: undefined,
  rejectedTotal: undefined,
};

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

describe('publicApplicationTabValues / privateApplicationTabValues', () => {
  it('mirrors the baseline ApplicationsTabs / PrivateApplicationTabs arrays exactly', () => {
    expect(publicApplicationTabValues()).toEqual(['latest', 'my-liked', 'trending', 'admin']);
    expect(privateApplicationTabValues()).toEqual(['all', 'drafts', 'published', 'moderation', 'approval', 'rejected']);
  });
});

describe('useApplicationTabs', () => {
  it('returns the four public tabs, in order, on the public project', () => {
    const { result } = renderHook(() => useApplicationTabs(true, TOTALS, true), { wrapper });
    expect(result.current.map((tab) => tab.value)).toEqual(['latest', 'my-liked', 'trending', 'admin']);
    expect(result.current[0]?.count).toBe(3);
  });

  it('hides the Admin tab when the caller lacks admin permission', () => {
    const { result } = renderHook(() => useApplicationTabs(true, TOTALS, false), { wrapper });
    const admin = result.current.find((tab) => tab.value === 'admin');
    expect(admin?.hidden).toBe(true);
  });

  it('hides the My liked and Trending placeholders, which have no data source yet', () => {
    const { result } = renderHook(() => useApplicationTabs(true, TOTALS, true), { wrapper });
    const hidden = result.current.filter((tab) => tab.hidden === true).map((tab) => tab.value);
    expect(hidden).toEqual(['my-liked', 'trending']);
  });

  it('shows the Admin tab when the caller has admin permission', () => {
    const { result } = renderHook(() => useApplicationTabs(true, TOTALS, true), { wrapper });
    const admin = result.current.find((tab) => tab.value === 'admin');
    expect(admin?.hidden).toBe(false);
  });

  it('returns the six private tabs, in order, on a private project', () => {
    const { result } = renderHook(() => useApplicationTabs(false, TOTALS, false), { wrapper });
    expect(result.current.map((tab) => tab.value)).toEqual([
      'all',
      'drafts',
      'published',
      'moderation',
      'approval',
      'rejected',
    ]);
    expect(result.current[0]?.count).toBe(5);
  });
});
