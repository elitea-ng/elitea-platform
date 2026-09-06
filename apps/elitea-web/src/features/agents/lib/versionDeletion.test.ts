import { describe, expect, it } from 'vitest';

import type { AgentPipelineVersionOption } from './types';
import { isDeleteVersionDisabled } from './versionDeletion';

function version(overrides: Partial<AgentPipelineVersionOption> = {}): AgentPipelineVersionOption {
  return { id: 2, name: 'v1', created_at: '2026-02-01T12:00:00Z', ...overrides };
}

describe('isDeleteVersionDisabled', () => {
  it('allows an ordinary named version', () => {
    expect(isDeleteVersionDisabled(version(), undefined)).toBe(false);
  });

  it('refuses the "base" version', () => {
    expect(isDeleteVersionDisabled(version({ id: 1, name: 'base' }), undefined)).toBe(true);
  });

  it('refuses the version that is the application default', () => {
    expect(isDeleteVersionDisabled(version({ id: 2 }), 2)).toBe(true);
  });

  it('allows a version while a DIFFERENT one is the default', () => {
    expect(isDeleteVersionDisabled(version({ id: 2 }), 7)).toBe(false);
  });

  /*
   * Not the same as "no default": a list that cannot say which version is the
   * default must not thereby make every version undeletable, nor make the
   * default deletable. `undefined` here means "no default recorded", and the
   * only rule that then applies is the "base" one above.
   */
  it('allows a named version when no default is recorded', () => {
    expect(isDeleteVersionDisabled(version(), undefined)).toBe(false);
  });

  it('refuses when there is no version to act on', () => {
    expect(isDeleteVersionDisabled(undefined, 2)).toBe(true);
  });

  /*
   * The published/embedded case belongs to the SERVER, deliberately. A client
   * copy would disagree with a status that changed since this menu rendered,
   * and the user would get a hidden item instead of the server's own
   * explanation. See the function's doc comment.
   */
  it('does not refuse a published version — that refusal is the server’s', () => {
    expect(isDeleteVersionDisabled(version({ status: 'published' }), undefined)).toBe(false);
  });
});
