import {
  APP_SWITCHER_BLUR,
  PrivacyProtection,
  SCREEN_CAPTURE_KEY,
  coverVisible,
} from '@/security/privacy/privacyProtection';

jest.mock('expo-screen-capture', () => ({
  preventScreenCaptureAsync: jest.fn(),
  allowScreenCaptureAsync: jest.fn(),
  enableAppSwitcherProtectionAsync: jest.fn(),
}));

function api() {
  return {
    preventScreenCaptureAsync: jest.fn(async () => {}),
    allowScreenCaptureAsync: jest.fn(async () => {}),
    enableAppSwitcherProtectionAsync: jest.fn(async () => {}),
  };
}

describe('PrivacyProtection', () => {
  it('iOS: enables the app-switcher blur once and blocks capture under the strict default', async () => {
    const a = api();
    const p = new PrivacyProtection('ios', a);
    await p.apply({ allow_screenshots: false });
    await p.apply({ allow_screenshots: false });
    expect(a.enableAppSwitcherProtectionAsync).toHaveBeenCalledTimes(1);
    expect(a.enableAppSwitcherProtectionAsync).toHaveBeenCalledWith(APP_SWITCHER_BLUR);
    expect(a.preventScreenCaptureAsync).toHaveBeenCalledTimes(1);
    expect(a.preventScreenCaptureAsync).toHaveBeenCalledWith(SCREEN_CAPTURE_KEY);
  });

  it('Android: sets FLAG_SECURE by default and never calls the iOS-only blur', async () => {
    const a = api();
    const p = new PrivacyProtection('android', a);
    await p.apply({ allow_screenshots: false });
    expect(a.preventScreenCaptureAsync).toHaveBeenCalledWith(SCREEN_CAPTURE_KEY);
    expect(a.enableAppSwitcherProtectionAsync).not.toHaveBeenCalled();
  });

  it('releases and re-applies the block when the policy toggles', async () => {
    const a = api();
    const p = new PrivacyProtection('android', a);
    await p.apply({ allow_screenshots: false });
    await p.apply({ allow_screenshots: true });
    await p.apply({ allow_screenshots: false });
    expect(a.preventScreenCaptureAsync).toHaveBeenCalledTimes(2);
    expect(a.allowScreenCaptureAsync).toHaveBeenCalledTimes(1);
    expect(a.allowScreenCaptureAsync).toHaveBeenCalledWith(SCREEN_CAPTURE_KEY);
    const order = [
      a.preventScreenCaptureAsync.mock.invocationCallOrder[0],
      a.allowScreenCaptureAsync.mock.invocationCallOrder[0],
      a.preventScreenCaptureAsync.mock.invocationCallOrder[1],
    ];
    expect([...order].sort((x, y) => x - y)).toEqual(order);
  });

  it('serialises quick toggles and ends in the last state', async () => {
    const a = api();
    const p = new PrivacyProtection('android', a);
    void p.apply({ allow_screenshots: false });
    void p.apply({ allow_screenshots: true });
    await p.apply({ allow_screenshots: true });
    expect(a.allowScreenCaptureAsync).toHaveBeenCalledTimes(1);
    expect(a.preventScreenCaptureAsync).toHaveBeenCalledTimes(1);
  });

  it('retries a failed call on the next apply and never throws', async () => {
    jest.spyOn(console, 'warn').mockImplementation(() => {});
    const a = api();
    a.preventScreenCaptureAsync.mockRejectedValueOnce(new Error('no window'));
    const p = new PrivacyProtection('android', a);
    await expect(p.apply({ allow_screenshots: false })).resolves.toBeUndefined();
    await p.apply({ allow_screenshots: false });
    expect(a.preventScreenCaptureAsync).toHaveBeenCalledTimes(2);
    jest.restoreAllMocks();
  });

  it('does nothing on web', async () => {
    const a = api();
    await new PrivacyProtection('web', a).apply({ allow_screenshots: false });
    expect(a.preventScreenCaptureAsync).not.toHaveBeenCalled();
    expect(a.enableAppSwitcherProtectionAsync).not.toHaveBeenCalled();
  });
});

describe('coverVisible', () => {
  it('covers inactive and background, not active', () => {
    expect(coverVisible('active')).toBe(false);
    expect(coverVisible('inactive')).toBe(true);
    expect(coverVisible('background')).toBe(true);
    expect(coverVisible(null)).toBe(false);
  });
});
