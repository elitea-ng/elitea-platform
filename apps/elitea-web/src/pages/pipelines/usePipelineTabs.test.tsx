import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { PipelinesTabTotals } from './usePipelinesData';
import { publicPipelineTabValues, usePipelineTabs } from './usePipelineTabs';

const TOTALS: PipelinesTabTotals = {
  latestTotal: 3,
  myLikedTotal: undefined,
  trendingTotal: undefined,
  applicationsTotal: 5,
};

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

describe('publicPipelineTabValues', () => {
  it('mirrors the baseline ApplicationsTabs array', () => {
    expect(publicPipelineTabValues()).toEqual(['latest', 'my-liked', 'trending', 'admin']);
  });
});

describe('usePipelineTabs', () => {
  it('returns the four public tabs, in order, on the public project', () => {
    const { result } = renderHook(() => usePipelineTabs(TOTALS, true), { wrapper });
    expect(result.current.map((tab) => tab.value)).toEqual(['latest', 'my-liked', 'trending', 'admin']);
    expect(result.current[0]?.count).toBe(3);
  });

  it('hides the Admin tab when the caller lacks admin permission', () => {
    const { result } = renderHook(() => usePipelineTabs(TOTALS, false), { wrapper });
    const admin = result.current.find((tab) => tab.value === 'admin');
    expect(admin?.hidden).toBe(true);
  });

  it('hides the My liked and Trending placeholders, which have no data source yet', () => {
    const { result } = renderHook(() => usePipelineTabs(TOTALS, true), { wrapper });
    const hidden = result.current.filter((tab) => tab.hidden === true).map((tab) => tab.value);
    expect(hidden).toEqual(['my-liked', 'trending']);
  });

  it('shows the Admin tab when the caller has admin permission', () => {
    const { result } = renderHook(() => usePipelineTabs(TOTALS, true), { wrapper });
    const admin = result.current.find((tab) => tab.value === 'admin');
    expect(admin?.hidden).toBe(false);
  });

  it('has no private tab: a private project\'s pipelines are one list (ADR-0051)', () => {
    const { result } = renderHook(() => usePipelineTabs(TOTALS, false), { wrapper });
    expect(result.current.map((tab) => tab.value)).not.toContain('all');
  });
});
