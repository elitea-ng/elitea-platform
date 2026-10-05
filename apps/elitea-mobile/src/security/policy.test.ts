import { STRICT_DEFAULTS, getPolicyState, resetPolicy, setPolicy } from '@/security/policy';

afterEach(() => resetPolicy());

describe('client policy', () => {
  it('starts from the strict defaults', () => {
    expect(getPolicyState()).toEqual({ policy: STRICT_DEFAULTS, source: 'default' });
    expect(STRICT_DEFAULTS).toMatchObject({
      require_device_lock: true,
      idle_lock_seconds: 300,
      allow_screenshots: false,
      offline_retention_days: 0,
    });
  });

  it('keeps the strict default for every invalid value', () => {
    setPolicy(
      {
        require_device_lock: 'no' as unknown as boolean,
        idle_lock_seconds: -5,
        allow_screenshots: 1 as unknown as boolean,
        offline_max_mb: 1.5,
      },
      'server',
    );
    expect(getPolicyState()).toEqual({ policy: STRICT_DEFAULTS, source: 'server' });
  });

  it('applies valid values', () => {
    setPolicy({ require_device_lock: false, idle_lock_seconds: 0, allow_screenshots: true }, 'discovery');
    expect(getPolicyState().policy).toMatchObject({
      require_device_lock: false,
      idle_lock_seconds: 0,
      allow_screenshots: true,
    });
  });
});
