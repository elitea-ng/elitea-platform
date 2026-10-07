/**
 * Type ramp guard (audit X2, docs/design-system.md §1.7): no screen or feature writes a
 * `fontSize` of its own. Text styles spread a role (`...ramp.body`, src/ui/type.ts) or render
 * `<Text variant>`; the only sizes live in the ramp (src/design/typography.ts) and the kit.
 */
declare const __dirname: string;
const fs = require('fs') as {
  readdirSync(path: string, options: { withFileTypes: true }): { name: string; isDirectory(): boolean }[];
  readFileSync(path: string, encoding: 'utf8'): string;
};
const path = require('path') as { join(...parts: string[]): string; relative(from: string, to: string): string };

const SRC = path.join(__dirname, '..');
const ALLOWED_DIRS = ['design', 'ui', 'test', 'api/generated'];

function sources(dir: string): string[] {
  const out: string[] = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    const rel = path.relative(SRC, full);
    if (entry.isDirectory()) {
      if (!ALLOWED_DIRS.includes(rel)) out.push(...sources(full));
    } else if (/\.(ts|tsx)$/.test(entry.name) && !/\.test\.(ts|tsx)$/.test(entry.name)) {
      out.push(full);
    }
  }
  return out;
}

describe('type ramp', () => {
  it('no source outside src/design and src/ui hard-codes a fontSize or lineHeight', () => {
    const offenders: string[] = [];
    for (const file of sources(SRC)) {
      fs.readFileSync(file, 'utf8')
        .split('\n')
        .forEach((line, i) => {
          if (/\b(fontSize|lineHeight)\s*:/.test(line)) offenders.push(`${path.relative(SRC, file)}:${i + 1}: ${line.trim()}`);
        });
    }
    expect(offenders).toEqual([]);
  });

  it('never sets text below 11 pt (captions ≥ 12, labels 11 for badges only)', () => {
    const { textStyles } = require('./type') as typeof import('./type');
    for (const platform of ['ios', 'android'] as const) {
      const styles = textStyles(platform);
      for (const [role, style] of Object.entries(styles)) {
        expect([role, style.fontSize! >= (role === 'label' ? 11 : 12)]).toEqual([role, true]);
        expect(style.lineHeight! >= style.fontSize!).toBe(true);
      }
    }
  });

  // F4: Android's subheadline and caption are medium (500) label roles; a static ramp entry that
  // carried that weight turned paragraph copy (`...ramp.subheadline`) medium on Android.
  it('static ramp entries carry no weight, so a spread role is regular unless it says otherwise', () => {
    const { textStyles } = require('./type') as typeof import('./type');
    for (const platform of ['ios', 'android'] as const) {
      for (const [role, style] of Object.entries(textStyles(platform))) {
        expect([platform, role, 'fontWeight' in style]).toEqual([platform, role, false]);
      }
    }
  });
});
