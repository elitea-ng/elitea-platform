import { describe, expect, it } from 'vitest';

import { formatWorkspaceBytes } from './AdminProjectsTable';

describe('formatWorkspaceBytes', () => {
  it('formats binary units and shows a dash for a missing value', () => {
    expect(formatWorkspaceBytes(undefined)).toBe('—');
    expect(formatWorkspaceBytes(-1)).toBe('—');
    expect(formatWorkspaceBytes(0)).toBe('0 B');
    expect(formatWorkspaceBytes(1023)).toBe('1023 B');
    expect(formatWorkspaceBytes(1536)).toBe('1.5 KiB');
    expect(formatWorkspaceBytes(200 * 1024 * 1024)).toBe('200 MiB');
    expect(formatWorkspaceBytes(3 * 1024 ** 3)).toBe('3.0 GiB');
  });
});
