/**
 * The splash sanitiser, at the boundary it exists for: markup an administrator
 * typed, rendered to every user a maintenance window refuses.
 *
 * Each refusal case is a way to run code WITHOUT a `<script>` tag, because the
 * server's own check is a substring scan and this one is the parse — the two
 * fail differently on purpose.
 */
import { describe, expect, it } from 'vitest';

import { sanitizeSplashHtml } from './sanitizeSplashHtml';

describe('sanitizeSplashHtml — what survives', () => {
  it('keeps ordinary body markup', () => {
    const html = '<p>We are upgrading.</p><ul><li>02:00 UTC</li></ul>';
    expect(sanitizeSplashHtml(html)).toContain('<p>We are upgrading.</p>');
    expect(sanitizeSplashHtml(html)).toContain('<li>02:00 UTC</li>');
  });

  it('keeps a link, including target — the operator points at a status page', () => {
    const out = sanitizeSplashHtml(
      '<a href="https://status.example.com" target="_blank" rel="noopener">Status</a>',
    );
    expect(out).toContain('href="https://status.example.com"');
    expect(out).toContain('target="_blank"');
  });

  it('keeps a table, which is the thing a plain sentence cannot carry', () => {
    const out = sanitizeSplashHtml('<table><tr><td>Start</td><td>02:00</td></tr></table>');
    expect(out).toContain('<td>Start</td>');
  });

  it('is empty for an empty body, so a caller can test the string it gets back', () => {
    expect(sanitizeSplashHtml('')).toBe('');
  });
});

describe('sanitizeSplashHtml — what does not', () => {
  const vectors: Record<string, string> = {
    'script tag': '<p>hi</p><script>window.pwned=1</script>',
    'img error handler': '<img src=x onerror="window.pwned=1">',
    'javascript url': '<a href="javascript:window.pwned=1">click</a>',
    iframe: '<iframe src="https://evil.example"></iframe>',
    'style block': '<style>body{display:none}</style>',
    'meta refresh': '<meta http-equiv="refresh" content="0;url=https://evil.example">',
    'base tag': '<base href="https://evil.example/">',
    'svg onload': '<svg onload="window.pwned=1"></svg>',
    'object embed': '<object data="evil.swf"></object>',
    'link stylesheet': '<link rel="stylesheet" href="https://evil.example/x.css">',
  };

  for (const [name, html] of Object.entries(vectors)) {
    it(`strips ${name}`, () => {
      const out = sanitizeSplashHtml(html);
      expect(out).not.toContain('pwned');
      expect(out).not.toContain('evil.example');
      expect(out.toLowerCase()).not.toMatch(
        /<(script|iframe|style|meta|base|svg|object|embed|link)\b/,
      );
      expect(out.toLowerCase()).not.toContain('onerror');
      expect(out.toLowerCase()).not.toContain('javascript:');
    });
  }

  it('keeps the surrounding prose when it strips a tag out of the middle of it', () => {
    // The failure mode a blunt "reject the whole value" would have: an operator
    // who pasted one bad tag loses everything they wrote.
    const out = sanitizeSplashHtml('<p>Before</p><script>x</script><p>After</p>');
    expect(out).toContain('Before');
    expect(out).toContain('After');
  });
});
