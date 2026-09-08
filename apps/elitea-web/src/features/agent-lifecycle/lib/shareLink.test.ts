import { describe, expect, it } from 'vitest';

import { buildEntityShareLink } from './shareLink';

describe('buildEntityShareLink', () => {
  it('builds an ENTITY link without a version segment', () => {
    expect(
      buildEntityShareLink({
        origin: 'http://localhost:8084',
        basename: '/app',
        entity: 'agents',
        tab: 'all',
        entityId: '7',
        name: 'Audit Agent',
      }),
    ).toBe('http://localhost:8084/app/agents/all/7?name=Audit%20Agent');
  });

  it('builds a VERSION link with the version segment', () => {
    expect(
      buildEntityShareLink({
        origin: 'http://localhost:8084',
        basename: '/app',
        entity: 'agents',
        tab: 'all',
        entityId: '7',
        versionId: '11',
        name: 'Audit Agent',
      }),
    ).toBe('http://localhost:8084/app/agents/all/7/11?name=Audit%20Agent');
  });

  it('serves pipelines from the pipelines route, not the agents one', () => {
    expect(
      buildEntityShareLink({
        origin: 'https://elitea.example',
        basename: '',
        entity: 'pipelines',
        tab: 'all',
        entityId: '3',
      }),
    ).toBe('https://elitea.example/pipelines/all/3');
  });

  // A basename with a trailing slash produced `…/app//agents/…`, which this
  // router does not match — the recipient would get a 404 from a link that
  // looked right.
  it('does not double the separator when the basename ends in a slash', () => {
    expect(
      buildEntityShareLink({
        origin: 'http://localhost:8084',
        basename: '/app/',
        entity: 'agents',
        tab: 'all',
        entityId: '7',
      }),
    ).toBe('http://localhost:8084/app/agents/all/7');
  });

  // The name reaches the URL, so a name with a `&` or a `#` in it must not be
  // able to add a query parameter or truncate the link.
  it('escapes a name that carries URL punctuation', () => {
    const link = buildEntityShareLink({
      origin: 'http://x',
      basename: '',
      entity: 'agents',
      tab: 'all',
      entityId: '1',
      name: 'A&B #1',
    });
    expect(link).toBe('http://x/agents/all/1?name=A%26B%20%231');
    expect(new URL(link).searchParams.get('name')).toBe('A&B #1');
  });

  it('omits the query entirely when there is no name', () => {
    expect(
      buildEntityShareLink({ origin: 'http://x', basename: '', entity: 'agents', tab: 'drafts', entityId: '1' }),
    ).toBe('http://x/agents/drafts/1');
  });
});
