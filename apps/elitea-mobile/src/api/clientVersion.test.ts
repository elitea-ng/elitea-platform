import { resolveClientVersion } from './clientVersion';

describe('resolveClientVersion', () => {
  it('prefers a plain semver native version', () => {
    expect(resolveClientVersion('1.4.2', '0.1.0')).toBe('1.4.2');
  });

  it('falls back to the config version when the native one is missing or not plain semver', () => {
    expect(resolveClientVersion(null, '0.1.0')).toBe('0.1.0');
    expect(resolveClientVersion('1.4', '0.1.0')).toBe('0.1.0');
    expect(resolveClientVersion('1.4.2-beta', '0.1.0')).toBe('0.1.0');
  });

  it('fails safe to 0.0.0', () => {
    expect(resolveClientVersion(undefined, undefined)).toBe('0.0.0');
  });
});
