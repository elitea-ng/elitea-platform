import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { grantsTurnedOff, isDesktopGrantsOff, isGrantsOffRefusal, markGrantsTurnedOff, resetGrantsTurnedOff, useGrantsTurnedOff } from './grantsOff';

afterEach(resetGrantsTurnedOff);

describe('grants turned off', () => {
  it('knows Main’s refusal with reason disabled, and no other one', () => {
    expect(isGrantsOffRefusal({ error: 'unattended_grant_refused', reason: 'disabled' })).toBe(true);
    expect(isGrantsOffRefusal({ error: 'unattended_grant_refused', reason: 'over_maximum' })).toBe(false);
    expect(isGrantsOffRefusal({ error: 'other', reason: 'disabled' })).toBe(false);
    expect(isGrantsOffRefusal('disabled')).toBe(false);
  });

  it('knows the desktop host’s refusal with reason disabled', () => {
    expect(isDesktopGrantsOff('unattended_grants_off')).toBe(true);
    expect(isDesktopGrantsOff('unattended_grant_refused')).toBe(false);
    expect(isDesktopGrantsOff('invalid_request')).toBe(false);
  });

  it('tells every subscriber once the flag is set', () => {
    const { result } = renderHook(() => useGrantsTurnedOff());
    expect(result.current).toBe(false);
    act(markGrantsTurnedOff);
    expect(result.current).toBe(true);
    expect(grantsTurnedOff()).toBe(true);
  });
});
