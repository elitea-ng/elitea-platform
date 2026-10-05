import type { AppStateStatus } from 'react-native';

import { createAppActivity } from '@/lib/appActivity';

function realAppState(initial: AppStateStatus = 'active') {
  const listeners = new Set<(s: AppStateStatus) => void>();
  const real = {
    currentState: initial as AppStateStatus,
    addEventListener: (_: 'change', l: (s: AppStateStatus) => void) => {
      listeners.add(l);
      return {
        remove: () => {
          listeners.delete(l);
        },
      };
    },
    emit: (s: AppStateStatus) => {
      real.currentState = s;
      [...listeners].forEach((l) => l(s));
    },
  };
  return real;
}

const tick = () => new Promise((r) => setTimeout(r, 0));

describe('appActivity', () => {
  it('reports a locked app as background and unlocking as active', async () => {
    const real = realAppState();
    const activity = createAppActivity(real);
    const seen: AppStateStatus[] = [];
    activity.addEventListener('change', (s) => seen.push(s));
    activity.setLocked(true);
    expect(activity.currentState).toBe('background');
    activity.setLocked(false);
    expect(seen).toEqual(['background', 'active']);
  });

  it('does not forward a resume that the lock closes in the same event', async () => {
    const real = realAppState();
    const activity = createAppActivity(real);
    const seen: AppStateStatus[] = [];
    activity.addEventListener('change', (s) => seen.push(s));
    real.emit('background');
    // The lock controller locks synchronously on the same 'active' event.
    real.emit('active');
    activity.setLocked(true);
    await tick();
    expect(seen).toEqual(['background']);
    activity.setLocked(false);
    expect(seen).toEqual(['background', 'active']);
  });

  it('forwards a plain background/foreground cycle', async () => {
    const real = realAppState();
    const activity = createAppActivity(real);
    const seen: AppStateStatus[] = [];
    activity.addEventListener('change', (s) => seen.push(s));
    real.emit('background');
    real.emit('active');
    await tick();
    expect(seen).toEqual(['background', 'active']);
  });
});
