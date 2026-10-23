import { describe, expect, it, vi } from 'vitest';

import { createMcpIpc, MCP_EVENT_CHANNEL, type McpProposalEvent } from './mcpIpc';
import { WorkspaceIpcError, type ListenFn } from './workspaceIpc';

const noListen: ListenFn = () => Promise.resolve(() => undefined);

describe('createMcpIpc', () => {
  it('maps every command to its host name and snake_case arguments', async () => {
    const invoke = vi.fn().mockResolvedValue(null);
    const ipc = createMcpIpc(invoke, noListen);

    await ipc.list('w1');
    await ipc.save('user', 'fs', { command: '/bin/fs' }, 'w1');
    await ipc.remove('workspace', 'fs', 'w1');
    await ipc.restart('w1', 'fs');
    await ipc.answerProposal('w1', 'abc', ['fs']);

    expect(invoke.mock.calls).toEqual([
      ['mcp_list', { workspace_id: 'w1' }],
      ['mcp_save', { scope: 'user', name: 'fs', workspace_id: 'w1', server: { command: '/bin/fs' } }],
      ['mcp_remove', { scope: 'workspace', name: 'fs', workspace_id: 'w1' }],
      ['mcp_restart', { workspace_id: 'w1', name: 'fs' }],
      ['mcp_proposal_answer', { workspace_id: 'w1', file_sha256: 'abc', accept: ['fs'] }],
    ]);
  });

  it('rejects with the host code', async () => {
    const ipc = createMcpIpc(vi.fn().mockRejectedValue({ code: 'mcp_not_started', message: 'later' }), noListen);
    const error: unknown = await ipc.restart('w1', 'fs').catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(WorkspaceIpcError);
    expect((error as WorkspaceIpcError).code).toBe('mcp_not_started');
  });

  it('hands over proposals from mcp://event', async () => {
    let deliver: (payload: unknown) => void = () => undefined;
    const listen: ListenFn = (channel, handler) => {
      expect(channel).toBe(MCP_EVENT_CHANNEL);
      deliver = handler;
      return Promise.resolve(() => undefined);
    };
    const seen: McpProposalEvent[] = [];
    await createMcpIpc(vi.fn(), listen).onProposal((event) => seen.push(event));
    deliver({ kind: 'something_else' });
    deliver({ kind: 'mcp_proposal', workspace_id: 'w1', file: '.mcp.json', file_sha256: 'a', servers: [] });
    expect(seen.map((event) => event.workspace_id)).toEqual(['w1']);
  });
});
