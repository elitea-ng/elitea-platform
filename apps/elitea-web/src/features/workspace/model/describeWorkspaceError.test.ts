import { describe, expect, it } from 'vitest';

import { WorkspaceIpcError } from '@/shared/desktop/workspaceIpc';

import { describeWorkspaceError } from './describeWorkspaceError';

describe('describeWorkspaceError', () => {
  it('says what to do for the codes a person can act on', () => {
    expect(describeWorkspaceError(new WorkspaceIpcError('workspace_busy', 'A turn is already running in this workspace.'))).toMatch(/still working/);
    expect(describeWorkspaceError({ code: 'agent_version_mismatch', message: 'raw' })).toMatch(/another version/);
    expect(describeWorkspaceError({ code: 'agent_not_in_conversation', message: 'raw' })).toMatch(/Start a new conversation/);
    expect(describeWorkspaceError({ code: 'turn_expired', message: 'raw' })).toMatch(/too old/);
  });

  it("shows the host's own message for any other code, and a fallback for none", () => {
    expect(describeWorkspaceError({ code: 'secrets_withheld', message: 'this agent needs secrets; run it in the cloud' })).toBe(
      'this agent needs secrets; run it in the cloud',
    );
    expect(describeWorkspaceError('plain host text')).toBe('plain host text');
    expect(describeWorkspaceError(new Error('network down'))).toBe('network down');
    expect(describeWorkspaceError({ code: 'x', message: '' })).toBe('That did not work. Try again.');
  });
});
