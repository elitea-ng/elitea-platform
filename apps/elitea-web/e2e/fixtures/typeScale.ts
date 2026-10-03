/**
 * Typography spec rev. 2 §4.6 — the type scale, measured on the real stack.
 *
 * The unit and Storybook tests prove the theme and the component overrides.
 * Neither can see a call site that sizes its own text, a third-party widget,
 * or a composition that inherits the wrong size; this walks what a browser
 * actually painted.
 *
 * `expectTypeScale` collects the computed `font-size` of every element that
 * directly holds visible text and asserts each one is a rung of the default
 * pack's ladder. `expectTextSpacingSurvives` injects the WCAG 1.4.12 text
 * spacing overrides and asserts that buttons, chips and nav items still show
 * their whole label.
 */
import { expect, type Page } from '@playwright/test';

/** The default pack's four rungs (`shared/brand/typography.ts`). */
export const LADDER_PX = [12, 14, 16, 20] as const;

/**
 * Surfaces that are NOT on the app's type scale by design. Each one is a
 * documented exception, not a convenience:
 *  - the sign-in page has no MUI and its own owner (spec §6 follow-up b);
 *  - CodeMirror and charts lay out glyphs in their own engines (a code
 *    editor's monospace metrics, a chart's SVG label fitting) rather than
 *    through the theme's variants.
 *
 * The support assistant is NOT exempt: its vendored CSS
 * (`widgets/support-assistant/vendor/theme/styles`) is mapped onto the ladder,
 * so the walk measures it like any other surface. (Its former selector,
 * `[data-testid^="support-assistant"]`, matched no element the widget renders.)
 */
const EXEMPT_ANCESTORS = [
  '[data-testid="sign-in-page"]',
  '.cm-editor',
  '.recharts-wrapper',
].join(', ');

interface Offender {
  readonly size: number;
  readonly tag: string;
  readonly text: string;
  readonly testId: string | null;
}

/** Every visible text-bearing element whose computed size is off the ladder. */
async function offLadder(page: Page): Promise<Offender[]> {
  return page.evaluate(
    ({ ladder, exempt }) => {
      const out: { size: number; tag: string; text: string; testId: string | null }[] = [];
      const seen = new Set<Element>();
      const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
        const text = node.textContent?.trim() ?? '';
        const element = node.parentElement;
        if (text === '' || element === null || seen.has(element)) continue;
        seen.add(element);
        if (element.closest(exempt) !== null) continue;
        if (element.closest('svg, script, style, noscript, [aria-hidden="true"]') !== null) continue;
        const style = getComputedStyle(element);
        if (style.visibility === 'hidden' || style.display === 'none' || Number(style.opacity) === 0) continue;
        const box = element.getBoundingClientRect();
        if (box.width === 0 || box.height === 0) continue;
        const size = Math.round(Number.parseFloat(style.fontSize) * 100) / 100;
        if (!ladder.includes(size)) {
          out.push({
            size,
            tag: element.tagName.toLowerCase(),
            text: text.slice(0, 40),
            testId: element.closest('[data-testid]')?.getAttribute('data-testid') ?? null,
          });
        }
      }
      return out;
    },
    { ladder: [...LADDER_PX] as number[], exempt: EXEMPT_ANCESTORS },
  );
}

/** Asserts every visible text on the page renders at a rung of the ladder. */
export async function expectTypeScale(page: Page, where: string): Promise<void> {
  const offenders = await offLadder(page);
  expect(offenders, `${where}: text rendered off the 12/14/16/20 ladder`).toEqual([]);
}

/** WCAG 1.4.12's four overrides, at the values the criterion names. */
const TEXT_SPACING_CSS = `
  * {
    line-height: 1.5 !important;
    letter-spacing: 0.12em !important;
    word-spacing: 0.16em !important;
  }
  p { margin-bottom: 2em !important; }
`;

/**
 * Injects the 1.4.12 text-spacing overrides and asserts no button, chip or
 * nav item clips its label vertically. Translated and narrow labels do wrap;
 * what must not happen is a fixed-height control hiding the wrapped line.
 */
export async function expectTextSpacingSurvives(page: Page, where: string): Promise<void> {
  const handle = await page.addStyleTag({ content: TEXT_SPACING_CSS });
  try {
    const clipped = await page.evaluate(() => {
      const controls = document.querySelectorAll<HTMLElement>(
        'button, [role="button"], [role="tab"], .MuiChip-label, nav a',
      );
      const out: { tag: string; text: string; scroll: number; client: number }[] = [];
      for (const control of controls) {
        const text = control.textContent?.trim() ?? '';
        if (text === '') continue;
        const style = getComputedStyle(control);
        if (style.display === 'none' || style.visibility === 'hidden') continue;
        if (control.getBoundingClientRect().height === 0) continue;
        // Only a control that hides overflow can clip; a visible overflow
        // spills, which 1.4.12 accepts.
        if (style.overflowY === 'visible' && style.overflow === 'visible') continue;
        if (control.scrollHeight > control.clientHeight + 1) {
          out.push({ tag: control.tagName.toLowerCase(), text: text.slice(0, 40), scroll: control.scrollHeight, client: control.clientHeight });
        }
      }
      return out;
    });
    expect(clipped, `${where}: controls clip their label under WCAG 1.4.12 text spacing`).toEqual([]);
  } finally {
    await handle.evaluate((node) => node.remove());
  }
}
