/**
 * The attribute and element contract shared by both HTML sanitisers
 * (`sanitizeMarkdownHtml` for chat/AI output, `sanitizeSplashHtml` for
 * operator splash markup).
 *
 * The rule under test: sanitised output carries no inline style, no event
 * handler, no form-submission attribute and no form control. The one
 * exception is the read-only task-list checkbox that GFM markdown renders, and
 * only for the markdown sanitiser.
 *
 * Every vector is parsed the way a browser parses it, so mixed-case names,
 * quoting tricks and nesting are covered by the parser, not by string checks.
 */
import { marked } from 'marked';
import { describe, expect, it } from 'vitest';

import { sanitizeMarkdownHtml } from './sanitizeMarkdownHtml';
import { sanitizeSplashHtml } from './sanitizeSplashHtml';

const sanitizers = {
  sanitizeMarkdownHtml,
  sanitizeSplashHtml,
} as const;

function parse(html: string): HTMLElement {
  const root = document.createElement('div');
  root.innerHTML = html;
  return root;
}

/** Names of every attribute on every element in `html`, lower-cased. */
function attributeNames(html: string): string[] {
  const names: string[] = [];
  for (const el of Array.from(parse(html).querySelectorAll('*'))) {
    for (const attr of Array.from(el.attributes)) names.push(attr.name.toLowerCase());
  }
  return names;
}

const FORM_ELEMENTS = ['form', 'input', 'button', 'select', 'textarea', 'option', 'fieldset', 'datalist'] as const;

const refused: Record<string, string> = {
  'style attribute': '<p style="position:fixed;inset:0;background:red">x</p>',
  'mixed-case STYLE attribute': '<p STYLE="color:red">x</p>',
  'mixed-case sTyLe attribute on an image': '<img src="https://example.com/a.png" sTyLe="width:9999px">',
  'style attribute on a link': '<a href="https://example.com" style="display:block">x</a>',
  'style attribute without quotes': '<p style=color:red>x</p>',
  'style element': '<style>body{display:none}</style><p>x</p>',
  'style element in mixed case': '<STYLE>body{display:none}</STYLE><p>x</p>',
  'style element nested in a table': '<table><tr><td><style>td{color:red}</style>x</td></tr></table>',
  'onerror handler': '<img src="x" onerror="window.pwned=1">',
  'onclick handler': '<p onclick="window.pwned=1">x</p>',
  'onload handler': '<img src="https://example.com/a.png" onload="window.pwned=1">',
  'onmouseover handler in mixed case': '<p OnMouseOver="window.pwned=1">x</p>',
  'onfocus with autofocus': '<p onfocus="window.pwned=1" tabindex="0" autofocus>x</p>',
  'ontoggle on details': '<details open ontoggle="window.pwned=1"><summary>x</summary></details>',
  'svg with style and onload': '<svg style="position:fixed" onload="window.pwned=1"><circle r="5"/></svg>',
  'svg animate': '<svg><animate onbegin="window.pwned=1" attributeName="x"/></svg>',
  'math with style': '<math style="color:red"><mi>x</mi></math>',
  'formaction on a button': '<form action="https://example.com/a"><button formaction="https://example.com/b">go</button></form>',
  'action on a form': '<form action="https://example.com/a" method="post"><input name="q"></form>',
  'form attribute linking a control': '<p form="f" id="x">x</p>',
  'form element': '<form><p>x</p></form>',
  'text input': '<input type="text" name="user" value="x">',
  'password input': '<input type="password" name="pw">',
  'hidden input': '<input type="hidden" name="csrf" value="x">',
  'submit input': '<input type="submit" formaction="https://example.com/a" value="go">',
  'button element': '<button type="submit">go</button>',
  'select element': '<select><option>a</option></select>',
  'textarea element': '<textarea>x</textarea>',
  'event handler on a data attribute name': '<p data-onclick="window.pwned=1" data-x="y">x</p>',
  'javascript url': '<a href="javascript:window.pwned=1">x</a>',
  'javascript url with entity-encoded scheme': '<a href="&#x6A;avascript:window.pwned=1">x</a>',
};

describe.each(Object.entries(sanitizers))('%s — refused constructs', (_name, sanitize) => {
  for (const [label, html] of Object.entries(refused)) {
    it(`leaves no ${label} in the output`, () => {
      const out = sanitize(html);
      const names = attributeNames(out);
      expect(names).not.toContain('style');
      expect(names.filter((n) => n.startsWith('on'))).toEqual([]);
      expect(names).not.toContain('formaction');
      expect(names).not.toContain('action');
      expect(names).not.toContain('form');
      expect(out).not.toContain('pwned');
      const root = parse(out);
      expect(root.querySelector('style')).toBeNull();
      for (const tag of FORM_ELEMENTS) expect(root.querySelector(tag)).toBeNull();
    });
  }

  it('drops class, id and name so markup cannot adopt application selectors or shadow document properties', () => {
    const out = sanitize('<div class="x" id="y" name="z">t</div>');
    expect(attributeNames(out)).toEqual([]);
    expect(out).toContain('t');
  });

  it('drops data-* attributes', () => {
    expect(attributeNames(sanitize('<p data-a="1" data-b="2">t</p>'))).toEqual([]);
  });

  it('keeps the text around a refused construct', () => {
    const out = sanitize('<p style="color:red">kept</p><form><button>x</button></form><p>also kept</p>');
    expect(out).toContain('kept');
    expect(out).toContain('also kept');
  });
});

describe('sanitizeMarkdownHtml — task-list checkbox', () => {
  it('keeps a disabled checkbox that carries only type, checked and disabled', () => {
    const out = sanitizeMarkdownHtml('<ul><li><input checked="" disabled="" type="checkbox"> done</li></ul>');
    const input = parse(out).querySelector('input');
    expect(input).not.toBeNull();
    expect(input?.getAttribute('type')).toBe('checkbox');
    expect(input?.hasAttribute('disabled')).toBe(true);
    expect(input?.hasAttribute('checked')).toBe(true);
    expect(Array.from(input?.attributes ?? []).map((a) => a.name).sort()).toEqual(['checked', 'disabled', 'type']);
  });

  it('strips every other attribute from a checkbox it keeps', () => {
    const out = sanitizeMarkdownHtml(
      '<input disabled type="checkbox" name="n" value="v" form="f" formaction="https://example.com" style="x:y" onclick="window.pwned=1" class="c" id="i">',
    );
    const input = parse(out).querySelector('input');
    expect(input).not.toBeNull();
    expect(Array.from(input?.attributes ?? []).map((a) => a.name).sort()).toEqual(['disabled', 'type']);
  });

  it('removes a checkbox that is not disabled', () => {
    expect(parse(sanitizeMarkdownHtml('<input type="checkbox">')).querySelector('input')).toBeNull();
  });

  it('removes a disabled input of any other type', () => {
    for (const type of ['text', 'radio', 'image', 'file', 'hidden', 'submit', 'password']) {
      expect(parse(sanitizeMarkdownHtml(`<input disabled type="${type}">`)).querySelector('input')).toBeNull();
    }
  });

  it('removes a checkbox type written in mixed case only when it is not a checkbox', () => {
    expect(parse(sanitizeMarkdownHtml('<input disabled type="CHECKBOX">')).querySelector('input')).not.toBeNull();
    expect(parse(sanitizeMarkdownHtml('<input disabled type="checkbox2">')).querySelector('input')).toBeNull();
  });

  it('keeps the checkbox that real GFM task-list markdown produces', () => {
    const html = marked.parse('- [x] done\n- [ ] todo\n', { async: false });
    const out = sanitizeMarkdownHtml(html);
    const boxes = Array.from(parse(out).querySelectorAll('input'));
    expect(boxes).toHaveLength(2);
    expect(boxes.map((b) => b.checked)).toEqual([true, false]);
    expect(boxes.every((b) => b.disabled && b.type === 'checkbox')).toBe(true);
  });

  it('does not let the splash sanitiser keep any input', () => {
    expect(parse(sanitizeSplashHtml('<input disabled type="checkbox">')).querySelector('input')).toBeNull();
  });
});

describe('sanitizeMarkdownHtml — ordinary markdown output survives', () => {
  const render = (md: string) => sanitizeMarkdownHtml(marked.parse(md, { async: false }));

  it('keeps headings, emphasis, lists and blockquotes byte for byte', () => {
    const md = '# H1\n\n## H2\n\nSome **bold** and *em* and `code`.\n\n- a\n- b\n\n1. one\n2. two\n\n> quote\n';
    const raw = marked.parse(md, { async: false });
    expect(render(md)).toBe(raw);
  });

  it('keeps a table byte for byte', () => {
    const raw = marked.parse('| a | b |\n|---|---|\n| 1 | 2 |\n', { async: false });
    expect(sanitizeMarkdownHtml(raw)).toBe(raw);
  });

  it('keeps a fenced code block and its text; the language class is not relied on by any renderer', () => {
    const out = render('```js\nconst a = 1 < 2;\n```\n');
    const code = parse(out).querySelector('pre > code');
    expect(code?.textContent).toBe('const a = 1 < 2;\n');
  });

  it('keeps a link with a safe href and title', () => {
    const out = render('[site](https://example.com/a?b=1 "T")');
    const a = parse(out).querySelector('a');
    expect(a?.getAttribute('href')).toBe('https://example.com/a?b=1');
    expect(a?.getAttribute('title')).toBe('T');
  });

  it('keeps an image with src, alt and title', () => {
    const out = render('![alt text](https://example.com/a.png "T")');
    const img = parse(out).querySelector('img');
    expect(img?.getAttribute('src')).toBe('https://example.com/a.png');
    expect(img?.getAttribute('alt')).toBe('alt text');
    expect(img?.getAttribute('title')).toBe('T');
  });

  it('keeps target=_blank with rel on a link', () => {
    const out = sanitizeMarkdownHtml('<a target="_blank" rel="noopener noreferrer" href="https://example.com">x</a>');
    expect(out).toContain('target="_blank"');
    expect(out).toContain('rel="noopener noreferrer"');
  });

  it('keeps aria attributes and table alignment', () => {
    const out = sanitizeMarkdownHtml('<table><tr><td align="right" aria-label="n">1</td></tr></table>');
    expect(out).toContain('aria-label="n"');
    expect(out).toContain('align="right"');
  });
});
