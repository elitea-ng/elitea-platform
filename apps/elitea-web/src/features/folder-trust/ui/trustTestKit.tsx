/** Test kit for the folder-trust components: an in-memory `TrustIpc` and a render with its provider and a query client. */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactElement } from 'react';
import { vi } from 'vitest';

import type { TrustIpc, TrustStatus } from '@/shared/desktop/trustIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { TrustIpcProvider } from '../model/useFolderTrust';

export const UNTRUSTED: TrustStatus = {
  workspace_id: 'w1',
  state: 'untrusted',
  suspended_by: null,
  tampered_file: null,
  allowed_by_policy: true,
  policy_blocks: [],
  inherited_from: null,
  grantable: true,
  ask_every_command: false,
  env_fallback: false,
  platform: 'macos',
  sandbox_available: true,
  granted_at: null,
  last_host_command_at: null,
  confirmation_version: 1,
};

export const TRUSTED: TrustStatus = { ...UNTRUSTED, state: 'trusted', granted_at: '2026-11-25T10:00:00Z' };

export function fakeTrustIpc(initial: TrustStatus) {
  let current = initial;
  const ipc = {
    status: vi.fn(() => Promise.resolve(current)),
    grant: vi.fn((_id: string, _version: number, ask: boolean) => {
      current = { ...current, state: 'trusted', ask_every_command: ask, granted_at: '2026-11-25T10:00:00Z' };
      return Promise.resolve(current);
    }),
    revoke: vi.fn(() => {
      current = { ...current, state: 'untrusted', suspended_by: null, tampered_file: null, granted_at: null };
      return Promise.resolve(current);
    }),
    list: vi.fn(() => Promise.resolve([{ ...current, name: 'repo', path: '/src/repo' }])),
    onEvent: vi.fn(() => Promise.resolve(() => undefined)),
  } satisfies TrustIpc;
  return ipc;
}

export function renderWithTrust(ui: ReactElement, ipc: TrustIpc) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithTheme(
    <QueryClientProvider client={client}>
      <TrustIpcProvider ipc={ipc}>{ui}</TrustIpcProvider>
    </QueryClientProvider>,
  );
}
