import { describe, expect, it } from 'vitest';

import { shellCrumbs } from './ShellTopBar';

describe('shellCrumbs', () => {
  it('is the section alone on a list page', () => {
    expect(shellCrumbs('/agents/all', [])).toEqual([{ label: 'Agents', to: '/agents' }]);
  });

  it('puts the section before the page own crumbs, which end in the heading', () => {
    expect(shellCrumbs('/toolkits/create', [{ label: 'New toolkit' }])).toEqual([
      { label: 'Toolkits', to: '/toolkits' },
      { label: 'New toolkit' },
    ]);
  });

  it('keeps the page crumbs alone outside every known section', () => {
    expect(shellCrumbs('/somewhere', [{ label: 'Somewhere' }])).toEqual([{ label: 'Somewhere' }]);
  });
});
