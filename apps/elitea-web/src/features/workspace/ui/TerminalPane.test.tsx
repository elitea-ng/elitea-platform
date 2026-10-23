import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { ProcessEvent, ProcessInfo, ProcessIpc } from '@/shared/desktop/processIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { terminals } from './__mocks__/xterm.mock';
import { TerminalPane } from './TerminalPane';

const SERVER: ProcessInfo = {
  process_id: 'p_00000001',
  call_id: 'c1',
  command: 'npm run dev',
  cwd: '.',
  tty: true,
  sandbox: 'workspace-write',
  network: false,
  loopback: true,
  keep_alive: 'turn',
  pinned: false,
  status: 'running',
  started_at: 1,
  bytes: 6,
};

function fakeIpc(processes: ProcessInfo[]) {
  const handlers = new Set<(event: ProcessEvent) => void>();
  const calls = {
    list: vi.fn((_workspaceId: string) => Promise.resolve(processes)),
    read: vi.fn((_workspaceId: string, _processId: string) => Promise.resolve({ data: 'ready\n', next_offset: 6, dropped_bytes: 0 })),
    write: vi.fn((_workspaceId: string, _processId: string, _data: string) => Promise.resolve()),
    resize: vi.fn((_workspaceId: string, _processId: string, _rows: number, _cols: number) => Promise.resolve()),
    kill: vi.fn((_workspaceId: string, _processId: string) => Promise.resolve()),
    pin: vi.fn((_workspaceId: string, _processId: string, _pinned: boolean) => Promise.resolve()),
  };
  const ipc: ProcessIpc = {
    ...calls,
    onEvent: (handler) => {
      handlers.add(handler);
      return Promise.resolve(() => handlers.delete(handler));
    },
  };
  return { ipc, calls, emit: (event: ProcessEvent) => handlers.forEach((handler) => handler(event)) };
}

describe('TerminalPane', () => {
  it('says so when the folder has no process', async () => {
    const { ipc, calls } = fakeIpc([]);
    renderWithTheme(<TerminalPane workspaceId="w1" ipc={ipc} />);
    await waitFor(() => expect(calls.list).toHaveBeenCalledWith('w1'));
    expect(screen.getByText('No background processes in this folder yet.')).toBeInTheDocument();
  });

  it('lists processes, pins and stops a running one', async () => {
    const { ipc, calls } = fakeIpc([SERVER, { ...SERVER, process_id: 'p_00000002', command: 'cargo test', status: 'exited', exit_code: 0 }]);
    renderWithTheme(<TerminalPane workspaceId="w1" ipc={ipc} />);
    expect(await screen.findByText('npm run dev')).toBeInTheDocument();
    expect(screen.getByText('Exited (0)')).toBeInTheDocument();
    expect(screen.getAllByTestId('process-row')).toHaveLength(2);

    await userEvent.click(screen.getByRole('button', { name: 'Pin' }));
    expect(calls.pin).toHaveBeenCalledWith('w1', 'p_00000001', true);
    await userEvent.click(screen.getByRole('button', { name: 'Stop' }));
    expect(calls.kill).toHaveBeenCalledWith('w1', 'p_00000001');
  });

  it('catches up, streams live output, sends keystrokes and keeps the list current', async () => {
    terminals.length = 0;
    const { ipc, calls, emit } = fakeIpc([SERVER]);
    renderWithTheme(<TerminalPane workspaceId="w1" ipc={ipc} />);
    await waitFor(() => expect(terminals.at(-1)?.written).toEqual(['ready\n']));
    expect(calls.read).toHaveBeenCalledWith('w1', 'p_00000001');

    const terminal = terminals.at(-1)!;
    act(() => {
      // Already held by the catch-up read: dropped.
      emit({ workspace_id: 'w1', seq: 1, kind: 'process_output', payload: { process_id: 'p_00000001', stream: 'pty', data: 'ready\n', offset: 0 } });
      emit({ workspace_id: 'w1', seq: 2, kind: 'process_output', payload: { process_id: 'p_00000001', stream: 'pty', data: 'GET /\n', offset: 6 } });
      emit({ workspace_id: 'w2', seq: 3, kind: 'process_output', payload: { process_id: 'p_00000001', stream: 'pty', data: 'other', offset: 12 } });
    });
    expect(terminal.written).toEqual(['ready\n', 'GET /\n']);

    terminal.type('q');
    expect(calls.write).toHaveBeenCalledWith('w1', 'p_00000001', 'q');

    act(() => emit({ workspace_id: 'w1', seq: 4, kind: 'process_exited', payload: { ...SERVER, status: 'killed' } }));
    expect(await screen.findByText('Stopped')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull();
  });
});
