import { createSplashGate } from '@/state/splashGate';

describe('createSplashGate', () => {
  it('hides only once everything is ready, and only once', () => {
    const hide = jest.fn(async () => {});
    const update = createSplashGate(hide);
    update(false);
    expect(hide).not.toHaveBeenCalled();
    update(true);
    update(true);
    update(false);
    expect(hide).toHaveBeenCalledTimes(1);
  });

  it('swallows a failing hide', () => {
    const update = createSplashGate(() => {
      throw new Error('no splash');
    });
    expect(() => update(true)).not.toThrow();
  });
});
