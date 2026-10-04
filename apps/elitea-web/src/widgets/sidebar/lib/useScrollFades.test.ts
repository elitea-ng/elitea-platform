import { describe, expect, it } from 'vitest';

import { scrollFadesOf } from './useScrollFades';

describe('scrollFadesOf (#6712)', () => {
  it('shows no fade when everything fits', () => {
    expect(scrollFadesOf({ scrollTop: 0, scrollHeight: 200, clientHeight: 200 })).toEqual({ showTop: false, showBottom: false });
  });

  it('shows only the bottom fade at the top of a long list', () => {
    expect(scrollFadesOf({ scrollTop: 0, scrollHeight: 600, clientHeight: 200 })).toEqual({ showTop: false, showBottom: true });
  });

  it('shows both fades in the middle of a long list', () => {
    expect(scrollFadesOf({ scrollTop: 150, scrollHeight: 600, clientHeight: 200 })).toEqual({ showTop: true, showBottom: true });
  });

  it('shows only the top fade at the bottom, with a fractional scrollTop', () => {
    expect(scrollFadesOf({ scrollTop: 399.5, scrollHeight: 600, clientHeight: 200 })).toEqual({ showTop: true, showBottom: false });
  });
});
