import { describe, expect, it } from 'vitest';

import { checkForm, EMPTY_FORM, formOf } from './serverForm';

describe('checkForm', () => {
  it('builds the server the host stores', () => {
    const { server, errors } = checkForm({
      ...EMPTY_FORM,
      name: 'github',
      command: ' /opt/homebrew/bin/github-mcp ',
      args: 'stdio\n\n--read-only\n',
      env: 'GITHUB_TOKEN=ghp_x=y\nLOG_LEVEL=',
      readOnlyTools: 'search, get_issue',
    });
    expect(errors).toEqual({});
    expect(server).toEqual({
      command: '/opt/homebrew/bin/github-mcp',
      args: ['stdio', '--read-only'],
      env: { GITHUB_TOKEN: 'ghp_x=y', LOG_LEVEL: '' },
      cwd: 'workspace',
      network: false,
      enabled: true,
      allowed_tools: null,
      read_only_tools: ['search', 'get_issue'],
      trust_read_only_hints: false,
    });
  });

  it('refuses what the schema refuses, field by field', () => {
    const { errors } = checkForm({
      ...EMPTY_FORM,
      name: 'my server',
      command: 'npx',
      args: Array.from({ length: 65 }, (_, i) => `a${i}`).join('\n'),
      env: 'lower=1',
    });
    expect(Object.keys(errors).sort()).toEqual(['args', 'command', 'env', 'name']);
    expect(checkForm({ ...EMPTY_FORM, name: 'x', command: '/bin/x', env: `A=${'v'.repeat(4097)}` }).errors.env).toEqual({ code: 'envValue', key: 'A' });
  });

  it('round-trips a stored server', () => {
    const stored = { command: '/bin/srv', args: ['-v'], env: { A: 'b' }, cwd: 'none' as const, network: true, allowed_tools: ['x', 'y'], read_only_tools: ['x'], trust_read_only_hints: true };
    const { server, errors } = checkForm(formOf('srv', 'workspace', stored));
    expect(errors).toEqual({});
    expect(server).toEqual({ ...stored, enabled: true });
  });
});
