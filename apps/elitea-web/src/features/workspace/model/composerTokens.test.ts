import { describe, expect, it } from 'vitest';

import { matchingCommands } from './composerCommands';
import { activeToken, mentionText, referencedPaths } from './composerTokens';

describe('activeToken', () => {
  it('finds an "@" token anywhere a word starts, up to the caret', () => {
    expect(activeToken('look at @src/ma', 15)).toEqual({ kind: 'file', start: 8, end: 15, query: 'src/ma' });
    expect(activeToken('@', 1)).toEqual({ kind: 'file', start: 0, end: 1, query: '' });
    expect(activeToken('a\n@x and more', 4)).toEqual({ kind: 'file', start: 2, end: 4, query: 'x' });
    expect(activeToken('mail me@example', 15)).toBeNull();
    expect(activeToken('@src ', 5)).toBeNull();
  });

  it('opens "/" only as the first word', () => {
    expect(activeToken('/pl', 3)).toEqual({ kind: 'command', start: 0, end: 3, query: 'pl' });
    expect(activeToken('  /', 3)).toEqual({ kind: 'command', start: 2, end: 3, query: '' });
    expect(activeToken('fix src/', 8)).toBeNull();
    expect(activeToken('do /plan', 8)).toBeNull();
  });
});

describe('referencedPaths', () => {
  it('keeps the picked paths still written as whole @tokens, in pick order, once', () => {
    const picked = ['src/main.rs', 'docs/', 'gone.txt', 'src/main.rs'];
    expect(referencedPaths('see @docs/ and @src/main.rs\n', picked)).toEqual(['src/main.rs', 'docs/']);
    expect(referencedPaths('see @src/main.rsx', ['src/main.rs'])).toEqual([]);
    expect(referencedPaths('mail@src/main.rs', ['src/main.rs'])).toEqual([]);
  });

  it('spells a folder with its slash', () => {
    expect(mentionText('docs', 'dir')).toBe('@docs/');
    expect(mentionText('a.txt', 'file')).toBe('@a.txt');
  });
});

describe('matchingCommands', () => {
  it('filters by prefix, case-insensitively', () => {
    expect(matchingCommands('').map((c) => c.name)).toEqual(['/new', '/plan', '/undo', '/agent', '/clear', '/help']);
    expect(matchingCommands('P').map((c) => c.id)).toEqual(['plan']);
    expect(matchingCommands('zzz')).toEqual([]);
  });
});
