/**
 * A prefix rule's pattern and examples as the panel's fields type them:
 * words separated by spaces, alternatives as `[test|check]` (or `test|check`);
 * one example per line.
 */
import type { PatternToken, PrefixRule, RuleExample } from '@/shared/desktop/rulesIpc';

export function formatPattern(pattern: readonly PatternToken[]): string {
  return pattern.map((token) => (typeof token === 'string' ? token : `[${token.join('|')}]`)).join(' ');
}

export function parsePattern(text: string): PatternToken[] {
  return text
    .trim()
    .split(/\s+/)
    .filter((word) => word !== '')
    .map((word) => {
      const inner = word.startsWith('[') && word.endsWith(']') ? word.slice(1, -1) : word;
      return inner.includes('|') ? inner.split('|') : inner;
    });
}

export function formatExample(example: RuleExample): string {
  return typeof example === 'string' ? example : example.join(' ');
}

export function parseExamples(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '');
}

/** "2 examples checked", for the list. */
export function exampleCount(rule: PrefixRule): number {
  return (rule.match?.length ?? 0) + (rule.not_match?.length ?? 0);
}
