// Speech is recognised on the device only (src/features/speech/dictation.ts,
// docs/design.md "Speech, on the device only"): Apple's server recogniser is
// never used. An App Intent that makes Siri ASK for free text ("What do you
// want to ask?") hands the user's spoken answer to Siri's own recogniser,
// which may run on Apple's servers depending on the device, the language and
// the Siri settings, with no workspace control over it (NS-SEC-1). So no
// intent may ask Siri to collect text: a String parameter is optional (filled
// only by a value the user put in a Shortcut), never required, and never
// carries a requestValueDialog. Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { describe, it } from 'node:test';

const dir = new URL('../app-intents/', import.meta.url);
const sources = readdirSync(dir)
  .filter((name) => name.endsWith('.swift'))
  .map((name) => ({ name, text: readFileSync(new URL(name, dir), 'utf8') }));

/** Every `@Parameter(...) var name: Type` declaration, comments stripped. */
function parameters(text) {
  const code = text.replace(/\/\/.*$/gm, '');
  return [...code.matchAll(/@Parameter\(([\s\S]*?)\)\s*var\s+(\w+)\s*:\s*([\w.?]+)/g)].map((m) => ({ args: m[1], name: m[2], type: m[3] }));
}

describe('App Intents never make Siri collect spoken text', () => {
  it('finds the intents', () => {
    assert.ok(sources.some((s) => /struct AskAgentIntent: AppIntent/.test(s.text)), 'AskAgentIntent not found');
  });

  for (const { name, text } of sources) {
    it(`${name}: no parameter asks Siri for a value`, () => {
      for (const p of parameters(text)) {
        assert.ok(!/requestValueDialog/.test(p.args), `${p.name} has a requestValueDialog`);
      }
      assert.ok(!/requestValue\s*\(/.test(text.replace(/\/\/.*$/gm, '')), 'a perform() calls requestValue');
    });

    it(`${name}: a text parameter is optional`, () => {
      for (const p of parameters(text)) {
        if (/^String\b/.test(p.type)) assert.ok(p.type.endsWith('?'), `${p.name}: ${p.type} is required, so Siri asks for it by voice`);
      }
    });
  }
});
