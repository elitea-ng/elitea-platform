/**
 * Saving the Features page's "Administration console" section redraws the
 * navigation (ADR-0059 R3.2, issue 2532): the console capability document
 * carries the section's `admin_console_areas` switch, so the save
 * invalidates the document; any other section's save leaves it alone.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getGetAdminConsoleDocumentQueryKey } from '@/shared/api/generated/admin/admin';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useSaveAdminConfigValues } from './adminConfigurationApi';

interface Mounted {
  readonly save: () => ReturnType<typeof useSaveAdminConfigValues>;
  readonly invalidated: () => readonly unknown[];
}

function mount(): Mounted {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const spy = vi.spyOn(queryClient, 'invalidateQueries');
  const wrapper = ({ children }: { children: ReactNode }): ReactNode => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
  const { result } = renderHook(() => useSaveAdminConfigValues(), { wrapper });
  return { save: () => result.current, invalidated: () => spy.mock.calls.map(([filters]) => filters?.queryKey) };
}

beforeEach(() => {
  vi.useRealTimers();
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
  server.use(http.put('*/admin/plugin_config_values/administration/:section', () => HttpResponse.json({})));
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useSaveAdminConfigValues and the console document', () => {
  it('invalidates the console document when the admin_console section is saved', async () => {
    const { save, invalidated } = mount();
    await act(() => save().mutateAsync({ sectionId: 'admin_console', values: { admin_console_areas: true } }));
    expect(invalidated()).toContainEqual(getGetAdminConsoleDocumentQueryKey());
  });

  it('leaves the console document alone when another section is saved', async () => {
    const { save, invalidated } = mount();
    await act(() => save().mutateAsync({ sectionId: 'analytics', values: { enabled: true } }));
    expect(invalidated()).not.toContainEqual(getGetAdminConsoleDocumentQueryKey());
    expect(invalidated().length).toBeGreaterThan(0);
  });
});
