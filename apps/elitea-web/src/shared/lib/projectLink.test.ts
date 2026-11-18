import { describe, expect, it } from 'vitest';

import { buildProjectLink, currentPageProjectLink } from './projectLink';

describe('buildProjectLink (#2148)', () => {
  it('puts the project segment between the basename and the page path', () => {
    expect(
      buildProjectLink({ origin: 'https://demo.elitea.ai', basename: '/app/', projectId: '17015', pathname: '/app/toolkits/all/12' }),
    ).toBe('https://demo.elitea.ai/app/17015/toolkits/all/12');
  });

  it('takes a path that carries no basename (dev) and keeps search and hash', () => {
    expect(
      buildProjectLink({
        origin: 'http://localhost:3000',
        basename: '',
        projectId: '5',
        pathname: '/toolkits/all/12',
        search: '?name=Docs',
        hash: '#indexes',
      }),
    ).toBe('http://localhost:3000/5/toolkits/all/12?name=Docs#indexes');
  });

  it('does not repeat a project segment the page already carries', () => {
    expect(
      buildProjectLink({ origin: 'https://h', basename: '/app', projectId: '5', pathname: '/app/5/toolkits/all/12' }),
    ).toBe('https://h/app/5/toolkits/all/12');
  });

  it('does not mistake a path that merely starts with the basename text for the basename', () => {
    expect(buildProjectLink({ origin: 'https://h', basename: '/app', projectId: '5', pathname: '/apple/x' })).toBe('https://h/app/5/apple/x');
  });
});

describe('currentPageProjectLink (#2148)', () => {
  it('falls back to the page address when no project is selected', () => {
    expect(currentPageProjectLink(undefined)).toBe(window.location.href);
    expect(currentPageProjectLink('')).toBe(window.location.href);
  });

  it('names the project when one is selected', () => {
    window.history.pushState({}, '', '/toolkits/all/12?name=x');
    expect(currentPageProjectLink('9')).toBe(`${window.location.origin}/9/toolkits/all/12?name=x`);
  });
});
