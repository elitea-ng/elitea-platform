import { marked, type MarkedToken } from 'marked';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { Token } from '.';

/** Lexes `markdown` and returns its single top-level token. */
function lexOne(markdown: string): MarkedToken {
  const [token] = marked.lexer(markdown);
  if (!token) throw new Error(`lexOne: ${markdown} produced no tokens`);
  return token as MarkedToken;
}

describe('Token', () => {
  it('renders a depth-1 heading as an <h1> with the headingLarge variant', () => {
    const { container } = renderWithTheme(<Token token={lexOne('# Title')} />);
    const h1 = container.querySelector('h1');
    expect(h1).not.toBeNull();
    expect(h1?.textContent).toBe('Title');
  });

  it('renders a depth-4 heading as an <h4> (collapsed to the headingSmall variant)', () => {
    const { container } = renderWithTheme(<Token token={lexOne('#### Title')} />);
    expect(container.querySelector('h4')?.textContent).toBe('Title');
  });

  it('renders a fenced code block as <pre><code>, unformatted', () => {
    const { container } = renderWithTheme(<Token token={lexOne('```\nconst x = 1 & 2;\n```')} />);
    const code = container.querySelector('pre > code');
    expect(code?.textContent).toBe('const x = 1 & 2;');
  });

  it('renders an unordered list with its items', () => {
    const { container } = renderWithTheme(<Token token={lexOne('- one\n- two')} />);
    expect(container.querySelector('ul')).not.toBeNull();
    expect(container.querySelectorAll('li')).toHaveLength(2);
    expect(container.textContent).toContain('one');
    expect(container.textContent).toContain('two');
  });

  it('preserves the starting number of an ordered list', () => {
    const { container } = renderWithTheme(<Token token={lexOne('4. four\n5. five')} />);
    expect(container.querySelector('ol')?.start).toBe(4);
  });

  it('renders a task list item with a disabled checkbox reflecting its checked state', () => {
    const { container } = renderWithTheme(<Token token={lexOne('- [x] done\n- [ ] not done')} />);
    const boxes = container.querySelectorAll('input[type="checkbox"]');
    expect(boxes).toHaveLength(2);
    expect((boxes[0] as HTMLInputElement).checked).toBe(true);
    expect((boxes[0] as HTMLInputElement).disabled).toBe(true);
    expect((boxes[1] as HTMLInputElement).checked).toBe(false);
    // a11y: a bare disabled checkbox has no accessible name without this.
    expect((boxes[0] as HTMLInputElement).getAttribute('aria-label')).toBe('Task complete');
    expect((boxes[1] as HTMLInputElement).getAttribute('aria-label')).toBe('Task incomplete');
  });

  it('defaults an absent checked state to false (defensive — real marked.lexer output always sets it explicitly)', () => {
    const { container } = renderWithTheme(
      <Token
        token={{
          type: 'list_item',
          raw: '- [ ] x',
          task: true,
          loose: false,
          text: 'x',
          tokens: [{ type: 'text', raw: 'x', text: 'x' }],
        }}
      />,
    );
    const box = container.querySelector('input[type="checkbox"]') as HTMLInputElement;
    expect(box.checked).toBe(false);
  });

  it('renders a blockquote, recursing into its nested paragraph', () => {
    const { container } = renderWithTheme(<Token token={lexOne('> quoted **bold** text')} />);
    const quote = container.querySelector('blockquote');
    expect(quote).not.toBeNull();
    expect(quote?.querySelector('strong')?.textContent).toBe('bold');
  });

  it('renders a table with a header row and body rows', () => {
    const { container } = renderWithTheme(
      <Token token={lexOne('| a | b |\n|---|---|\n| 1 | 2 |')} />,
    );
    expect(container.querySelectorAll('th')).toHaveLength(2);
    expect(container.querySelectorAll('td')).toHaveLength(2);
    expect(container.querySelector('th')?.getAttribute('scope')).toBe('col');
  });

  it('renders an <hr> for a thematic break', () => {
    const { container } = renderWithTheme(<Token token={lexOne('---')} />);
    expect(container.querySelector('hr')).not.toBeNull();
  });

  it('renders nothing for a space token', () => {
    const { container } = renderWithTheme(<Token token={{ type: 'space', raw: '\n\n' }} />);
    expect(container.firstElementChild?.textContent ?? '').toBe('');
  });

  it('renders a <br> for a br token reached directly (a hard line break, normally resolved inline via DefaultMarkdown)', () => {
    const { container } = renderWithTheme(<Token token={{ type: 'br', raw: '\n' }} />);
    expect(container.querySelector('br')).not.toBeNull();
  });

  it('renders nothing for a checkbox token reached directly (it is normally absorbed by its list_item)', () => {
    const { container } = renderWithTheme(
      <Token token={{ type: 'checkbox', raw: '[x] ', checked: true }} />,
    );
    expect(container.querySelector('input')).toBeNull();
  });

  it('falls back to re-parsing raw markdown for a leaf inline token reached directly (defensive branch)', () => {
    // 'strong' tokens are normally absorbed into their parent paragraph's
    // single DefaultMarkdown call and never reach Token directly — this
    // covers the defensive fallback branch `typescript/switch-exhaustiveness-
    // check` requires Token to have.
    const { container } = renderWithTheme(
      <Token token={{ type: 'strong', raw: '**bold**', text: 'bold', tokens: [] }} />,
    );
    expect(container.querySelector('strong')?.textContent).toBe('bold');
  });

  it('renders raw HTML block tokens sanitized (defence in depth for the html case)', () => {
    const { container } = renderWithTheme(<Token token={lexOne('<script>alert(1)</script>')} />);
    expect(container.innerHTML).not.toContain('<script');
  });

  it('forwards renderHtml=false through recursive list rendering', () => {
    const { container } = renderWithTheme(
      <Token
        token={lexOne('- item with <b>html</b>')}
        renderHtml={false}
      />,
    );
    expect(container.querySelector('b')).toBeNull();
    expect(container.textContent).toContain('html');
  });
});
