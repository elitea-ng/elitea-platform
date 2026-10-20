import { compareVersions, isBelowMinimum, parseVersion } from '@/lib/semver';

describe('parseVersion', () => {
  it.each(['1.2.3', 'v1.2.3', '1.2.3-rc.1', '1.2.3+build.5', '1.2.3-beta+exp.sha.5114f85', '01.02.03'])(
    'accepts %s',
    (text) => {
      expect(parseVersion(text)).not.toBeNull();
    },
  );

  it.each(['', '1.2', '1.2.3.4', '1.2.x', '1.2.3-', '1.2.3-a..b', '1.2.3+', 'latest', '1234567890.0.0', 'x'.repeat(65)])(
    'rejects %p',
    (text) => {
      expect(parseVersion(text)).toBeNull();
    },
  );
});

describe('compareVersions (SemVer 2.0.0 §11)', () => {
  const ordered = [
    '1.0.0-alpha',
    '1.0.0-alpha.1',
    '1.0.0-alpha.beta',
    '1.0.0-beta',
    '1.0.0-beta.2',
    '1.0.0-beta.11',
    '1.0.0-rc.1',
    '1.0.0',
    '1.0.1',
    '1.1.0',
    '2.0.0',
  ];
  it('orders the specification example list', () => {
    for (let i = 0; i < ordered.length - 1; i++) {
      const a = parseVersion(ordered[i])!;
      const b = parseVersion(ordered[i + 1])!;
      expect([ordered[i], compareVersions(a, b)]).toEqual([ordered[i], -1]);
      expect(compareVersions(b, a)).toBe(1);
    }
  });

  it('ignores build metadata and a leading v', () => {
    expect(compareVersions(parseVersion('v1.2.3+7')!, parseVersion('1.2.3')!)).toBe(0);
  });
});

describe('isBelowMinimum', () => {
  it('has no minimum when it is empty or unparsable', () => {
    expect(isBelowMinimum('0.1.0', '')).toBe(false);
    expect(isBelowMinimum('0.1.0', undefined)).toBe(false);
    expect(isBelowMinimum('0.1.0', 'soon')).toBe(false);
  });

  it('compares against a real minimum', () => {
    expect(isBelowMinimum('0.1.0', '0.2.0')).toBe(true);
    expect(isBelowMinimum('0.2.0', '0.2.0')).toBe(false);
    expect(isBelowMinimum('0.2.0-rc.1', '0.2.0')).toBe(true);
    expect(isBelowMinimum('1.0.0', '0.9.9')).toBe(false);
  });

  it('treats an unparsable own version as too old', () => {
    expect(isBelowMinimum('dev', '0.1.0')).toBe(true);
  });
});
