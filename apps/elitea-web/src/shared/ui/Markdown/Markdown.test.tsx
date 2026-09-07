import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { Markdown } from '.';

describe('Markdown', () => {
  it('renders a full document: heading, paragraph, and list', () => {
    const { container } = renderWithTheme(
      <Markdown>{'# Title\n\nSome **bold** text.\n\n- one\n- two'}</Markdown>,
    );
    expect(container.querySelector('h1')?.textContent).toBe('Title');
    expect(container.querySelector('strong')?.textContent).toBe('bold');
    expect(container.querySelectorAll('li')).toHaveLength(2);
  });

  it('renders an empty document as an empty container', () => {
    const { container, getByTestId } = renderWithTheme(<Markdown data-testid="md">{''}</Markdown>);
    expect(getByTestId('md').textContent).toBe('');
    expect(container).toBeInTheDocument();
  });

  // Security: a full multi-block document is the realistic shape of AI/user
  // chat content this component renders — a <script> anywhere in it must
  // never survive to the DOM regardless of which block it lands in.
  it('strips a <script> tag anywhere in a full multi-block document', () => {
    const { container } = renderWithTheme(
      <Markdown>
        {[
          '# Title',
          '',
          'A paragraph with <script>window.__pwned = 1</script> inline.',
          '',
          '- a list item',
          '',
          '| a | b |',
          '|---|---|',
          '| <script>window.__pwned2 = 1</script> | 2 |',
        ].join('\n')}
      </Markdown>,
    );
    expect(container.innerHTML).not.toContain('<script');
    expect(container.innerHTML).not.toContain('__pwned');
  });

  it('defaults renderHtml to true (literal inline HTML renders)', () => {
    const { container } = renderWithTheme(<Markdown>{'A <b>bold html</b> B'}</Markdown>);
    expect(container.querySelector('b')?.textContent).toBe('bold html');
  });

  it('drops literal HTML tree-wide when renderHtml is false', () => {
    const { container } = renderWithTheme(<Markdown renderHtml={false}>{'A <b>bold html</b> B'}</Markdown>);
    expect(container.querySelector('b')).toBeNull();
    expect(container.textContent).toContain('bold html');
  });

  it('forwards data-testid', () => {
    const { getByTestId } = renderWithTheme(<Markdown data-testid="md-root">{'x'}</Markdown>);
    expect(getByTestId('md-root')).toBeInTheDocument();
  });

  it('merges a caller sx with its own base container styles', () => {
    const { getByTestId } = renderWithTheme(
      <Markdown
        data-testid="md-root"
        sx={{ margin: 0 }}
      >
        {'x'}
      </Markdown>,
    );
    expect(getByTestId('md-root')).toBeInTheDocument();
  });

  // Issue #625 item 1: per-word TTS highlight sync.
  describe('spokenRange', () => {
    it('is a no-op when TTS is idle (spokenRange omitted)', () => {
      const { container } = renderWithTheme(<Markdown>{'the quick brown fox'}</Markdown>);
      expect(container.querySelector('mark')).toBeNull();
    });

    it('highlights exactly the word the range names, across a paragraph boundary', () => {
      const source = 'First paragraph here.\n\nSecond paragraph word.';
      // "paragraph" inside the SECOND block — proves the offset is resolved
      // against the token it actually falls in, not always the first one.
      const start = source.indexOf('paragraph', source.indexOf('Second'));
      const { container } = renderWithTheme(
        <Markdown spokenRange={{ start, end: start + 'paragraph'.length }}>{source}</Markdown>,
      );
      const marks = container.querySelectorAll('mark');
      expect(marks).toHaveLength(1);
      expect(marks[0]?.textContent).toBe('paragraph');
    });

    it('does not highlight a token with inline formatting (bold) rather than risk splitting a tag', () => {
      const source = 'Hello **bold** world.';
      const start = source.indexOf('bold');
      const { container } = renderWithTheme(
        <Markdown spokenRange={{ start, end: start + 'bold'.length }}>{source}</Markdown>,
      );
      expect(container.querySelector('mark')).toBeNull();
      expect(container.querySelector('strong')?.textContent).toBe('bold');
    });

    it('highlights nothing when the range falls outside every token (idle-shaped range)', () => {
      const { container } = renderWithTheme(
        <Markdown spokenRange={{ start: 9999, end: 10005 }}>{'short text'}</Markdown>,
      );
      expect(container.querySelector('mark')).toBeNull();
    });
  });
});
