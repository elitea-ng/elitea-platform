import { describe, expect, it } from 'vitest';

import { bugReportUrl, whatsNewUrl } from '../lib/accountLinks';

describe('bugReportUrl', () => {
  it('prefills a public issue with the version, platform and language only', () => {
    const url = new URL(bugReportUrl({ version: '7.3.0-nightly.20261027.509', userAgent: 'Mozilla/5.0 (Macintosh)', locale: 'fr-CA' }));
    expect(`${url.origin}${url.pathname}`).toBe('https://github.com/elitea-ng/elitea-platform/issues/new');
    expect(url.searchParams.get('labels')).toBe('bug');
    expect(url.searchParams.get('title')).toBe('[desktop] ');
    const body = url.searchParams.get('body') ?? '';
    expect(body).toContain('- Elitea desktop: 7.3.0-nightly.20261027.509');
    expect(body).toContain('- Platform: Mozilla/5.0 (Macintosh)');
    expect(body).toContain('- Language: fr-CA');
    expect(body).toContain('This repository is public');
  });

  it('says "unknown" outside the desktop app', () => {
    const body = new URL(bugReportUrl({ version: undefined, userAgent: 'x', locale: 'en' })).searchParams.get('body');
    expect(body).toContain('- Elitea desktop: unknown');
  });
});

describe('whatsNewUrl', () => {
  it('opens a stable release by its tag', () => {
    expect(whatsNewUrl('7.2.0')).toBe('https://github.com/elitea-ng/elitea-platform/releases/tag/v7.2.0');
  });

  it('opens a pre-release at its anchor on the list, which survives the prune', () => {
    expect(whatsNewUrl('7.3.0-nightly.20261027.509')).toBe('https://github.com/elitea-ng/elitea-platform/releases#release-v7.3.0-nightly.20261027.509');
  });

  it('lists the releases for a local build or no version', () => {
    expect(whatsNewUrl('0.1.0')).toBe('https://github.com/elitea-ng/elitea-platform/releases');
    expect(whatsNewUrl(undefined)).toBe('https://github.com/elitea-ng/elitea-platform/releases');
    expect(whatsNewUrl('not a version')).toBe('https://github.com/elitea-ng/elitea-platform/releases');
  });
});
