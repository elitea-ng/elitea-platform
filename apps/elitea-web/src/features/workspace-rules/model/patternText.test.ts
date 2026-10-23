import { describe, expect, it } from 'vitest';

import type { PatternToken } from '@/shared/desktop/rulesIpc';

import { formatExample, formatPattern, parsePattern, PatternSyntaxError } from './patternText';

describe('patternText', () => {
  it('reads words, alternatives and the wildcard', () => {
    expect(parsePattern('cargo [test|check] --all')).toEqual(['cargo', ['test', 'check'], '--all']);
    expect(parsePattern('  git   test|check ')).toEqual(['git', ['test', 'check']]);
    expect(parsePattern('git * --force')).toEqual(['git', { any: true }, '--force']);
    expect(parsePattern("echo '*'")).toEqual(['echo', '*']);
    expect(parsePattern('echo \\*')).toEqual(['echo', '*']);
  });

  it('keeps token boundaries through quoting', () => {
    expect(parsePattern("'/Users/me/My Tools/run' build")).toEqual(['/Users/me/My Tools/run', 'build']);
    expect(parsePattern('"a b" c\\ d')).toEqual(['a b', 'c d']);
    expect(parsePattern("grep 'a|b'")).toEqual(['grep', 'a|b']);
    expect(parsePattern("['a|b'|c]")).toEqual([['a|b', 'c']]);
    expect(parsePattern("'it'\"'\"'s'")).toEqual(["it's"]);
    expect(parsePattern('say "quote \\" inside"')).toEqual(['say', 'quote " inside']);
  });

  it('round-trips every pattern', () => {
    const patterns: PatternToken[][] = [
      ['cargo', ['test', 'check']],
      ['/Users/me/My Tools/run', 'build'],
      ['grep', 'a|b', '[x]', "it's", 'tab\there', ''],
      ['git', { any: true }, '--force'],
      ['echo', '*', ['*', 'x y', 'p|q']],
      ['naïve', 'ünïcode'],
    ];
    for (const pattern of patterns) {
      expect(parsePattern(formatPattern(pattern))).toEqual(pattern);
    }
    expect(formatPattern(['git', { any: true }, '*'])).toBe("git * '*'");
    expect(formatPattern(['cargo', ['test', 'check']])).toBe('cargo [test|check]');
  });

  it('refuses unclosed quotes and brackets', () => {
    expect(() => parsePattern("git 'reset")).toThrow(PatternSyntaxError);
    expect(() => parsePattern('git "reset')).toThrow(PatternSyntaxError);
    expect(() => parsePattern('git [reset|log')).toThrow(PatternSyntaxError);
    expect(() => parsePattern('git [reset]x')).toThrow(PatternSyntaxError);
  });

  it('writes argv examples as shell-quoted lines', () => {
    expect(formatExample(['ls', '-la', 'My Docs'])).toBe("ls -la 'My Docs'");
    expect(formatExample('cargo test -p x')).toBe('cargo test -p x');
    expect(formatExample(['grep', 'a|b'])).toBe("grep 'a|b'");
  });
});
