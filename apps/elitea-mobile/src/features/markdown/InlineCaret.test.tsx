import { render, screen } from '@testing-library/react-native';

import { colors } from '@/design/tokens';
import { CARET_MARK, dimCaretColor, withCaretMark } from '@/features/markdown/InlineCaret';
import { Markdown } from '@/features/markdown/Markdown';
import { composite, contrastRatio, parseColor } from '@/theme/color';

const M = CARET_MARK;

describe('withCaretMark', () => {
  it.each([
    ['Hello there', `Hello there${M}`],
    ['Hello there\n\n', `Hello there${M}`],
    ['- one\n- two', `- one\n- two${M}`],
    ['```py\nx = 1', `\`\`\`py\nx = 1${M}`],
    ['```py', `\`\`\`py\n${M}`],
    ['```py\nx = 1\n```', `\`\`\`py\nx = 1\n\`\`\`\n${M}`],
    ['Title\n---', `Title\n---\n\n${M}`],
    ['| a | b |\n|---|---|', `| a | b |\n|---|---|\n\n${M}`],
  ])('%j', (source, want) => {
    expect(withCaretMark(source)).toBe(want);
  });
});

describe('the inline streaming caret (docs/design-system.md §10)', () => {
  type Json = { type: string; props: Record<string, unknown>; children: (Json | string)[] | null };
  const texts = (node: Json | string): string => (typeof node === 'string' ? node : (node.children ?? []).map(texts).join(''));
  const hasCaret = (node: Json | string): boolean =>
    typeof node !== 'string' && (node.props.testID === 'streaming-caret' || (node.children ?? []).some(hasCaret));
  /** The text of the innermost Text run that holds the caret: the line the caret is drawn on. */
  const caretRun = (): string | null => {
    let found: string | null = null;
    const walk = (node: Json | string) => {
      if (typeof node === 'string') return;
      if (node.type === 'Text' && hasCaret(node) && node.props.testID !== 'streaming-caret') {
        const own = (node.children ?? []).filter((c) => typeof c === 'string' || !hasCaret(c)).map(texts).join('');
        if (own.trim() !== '') found = own;
      }
      (node.children ?? []).forEach(walk);
    };
    const tree = screen.toJSON() as Json | Json[];
    (Array.isArray(tree) ? tree : [tree]).forEach(walk);
    return found;
  };

  it('sits inside the last paragraph, after the last word, not on a line of its own', async () => {
    await render(<Markdown value={'First paragraph.\n\nSecond one ends here'} caret />);
    expect(caretRun()).toBe('Second one ends here');
    expect(screen.getAllByTestId('streaming-caret')).toHaveLength(1);
  });

  it('follows the code inside an open code fence, and the code it copies has no caret', async () => {
    await render(<Markdown value={'```py\nx = 1'} caret />);
    expect(caretRun()).toBe('x = 1');
    expect(JSON.stringify(screen.toJSON())).not.toContain(M);
  });

  it('is not drawn when the answer is done', async () => {
    await render(<Markdown value="Done." />);
    expect(screen.queryByTestId('streaming-caret')).toBeNull();
  });

  it.each(['light', 'dark'] as const)('%s: the dim phase still holds 3:1 on the answer bubble', (scheme) => {
    const { accent, bubbleAgent } = colors[scheme];
    const dim = parseColor(dimCaretColor(accent, bubbleAgent))!;
    expect(contrastRatio(dim, parseColor(bubbleAgent)!)).toBeGreaterThanOrEqual(3);
    // and it is visibly dimmer than the accent
    expect(dim).not.toEqual(composite(parseColor(accent)!, parseColor(bubbleAgent)!));
  });
});
