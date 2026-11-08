// ADR-0052 W1.4: the App Intents name no product. Their titles, descriptions and
// phrases are extracted from Swift constants at BUILD time (Apple's App Intents
// metadata), so they cannot follow the brand at run time. Instead they say
// nothing brand-specific, and the phrases use `\(.applicationName)`, which iOS
// fills with the installed app's own name (CFBundleDisplayName, i.e. the
// brand's productName). Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { describe, it } from 'node:test';

const dir = new URL('../app-intents/', import.meta.url);
const sources = readdirSync(dir)
  .filter((name) => name.endsWith('.swift'))
  .map((name) => ({ name, text: readFileSync(new URL(name, dir), 'utf8') }));

/** Every string literal in the code (comments stripped). */
const literals = (text) => [...text.replace(/\/\/.*$/gm, '').matchAll(/"((?:[^"\\]|\\.)*)"/g)].map((m) => m[1]);

describe('App Intents are brand-neutral', () => {
  it('finds the intents and the shortcuts provider', () => {
    assert.ok(sources.some((s) => /struct NewChatIntent: AppIntent/.test(s.text)));
    assert.ok(sources.some((s) => /: AppShortcutsProvider/.test(s.text)));
  });

  for (const { name, text } of sources) {
    it(`${name}: no string names ELITEA or EPAM`, () => {
      for (const lit of literals(text)) assert.doesNotMatch(lit, /elitea|epam/i, `${name}: "${lit}"`);
    });
    it(`${name}: no type is named after ELITEA`, () => {
      assert.doesNotMatch(text.replace(/\/\/.*$/gm, ''), /\b(struct|class|enum|protocol)\s+Elitea\w*/);
    });
  }

  it('every shortcut phrase names the app through \\(.applicationName)', () => {
    const provider = sources.map((s) => s.text).join('\n');
    const phrases = [...provider.matchAll(/phrases:\s*\[([\s\S]*?)\]/g)].flatMap((m) => literals(m[1]));
    assert.ok(phrases.length >= 3, 'no phrases found');
    for (const p of phrases) assert.match(p, /\\\(\.applicationName\)/, `phrase "${p}" does not name the app`);
  });
});
