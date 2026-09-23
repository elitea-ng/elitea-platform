/**
 * The pure half of the selected-project seam. The hook over it is one line and
 * needs a real router; this is the part with the branches.
 */
import { describe, expect, it } from 'vitest';

import { selectProjectId } from './useSelectedProjectId';

describe('selectProjectId', () => {
  it('reads the id the router context resolves', () => {
    expect(selectProjectId({ auth: { getSelectedProjectId: () => '42' } })).toBe('42');
  });

  it('answers undefined for a context that carries no accessor', () => {
    // Not a THROW: `strict: false` gives a context whose shape this widget
    // does not control, and the attach control it feeds must still render.
    expect(selectProjectId({})).toBeUndefined();
    expect(selectProjectId({ auth: {} })).toBeUndefined();
  });

  it('answers undefined for a non-object context', () => {
    expect(selectProjectId(undefined)).toBeUndefined();
    expect(selectProjectId(null)).toBeUndefined();
    expect(selectProjectId('nope')).toBeUndefined();
  });
});
