import { act, fireEvent, render, screen } from '@testing-library/react-native';

import { CodeBlock } from '@/features/markdown/CodeBlock';

jest.mock('expo-clipboard', () => ({ setStringAsync: jest.fn(async () => true) }));

describe('CodeBlock overflow (a long line says it goes on)', () => {
  const layout = async (box: number, content: number) => {
    const scroll = screen.getByTestId('markdown-code-scroll');
    await act(async () => {
      fireEvent(scroll, 'layout', { nativeEvent: { layout: { width: box, height: 40, x: 0, y: 0 } } });
      fireEvent(scroll, 'contentSizeChange', content, 40);
    });
    return scroll;
  };

  it('a line that fits: no fade, no indicator', async () => {
    await render(<CodeBlock code="x = 1" language="py" selectable />);
    const scroll = await layout(300, 120);
    expect(screen.queryByTestId('markdown-code-fade')).toBeNull();
    expect(scroll.props.showsHorizontalScrollIndicator).toBe(false);
  });

  it('a wider line: the indicator and a trailing fade, which clears at the end', async () => {
    await render(<CodeBlock code={'rm -rf build/ && '.repeat(10)} language="sh" selectable />);
    const scroll = await layout(300, 900);
    expect(screen.getByTestId('markdown-code-fade')).toBeTruthy();
    expect(scroll.props.showsHorizontalScrollIndicator).toBe(true);
    expect(scroll.props.accessibilityHint).toMatch(/sideways/);
    await act(async () => {
      fireEvent.scroll(scroll, {
        nativeEvent: { contentOffset: { x: 600, y: 0 }, contentSize: { width: 900, height: 40 }, layoutMeasurement: { width: 300, height: 40 } },
      });
    });
    expect(screen.queryByTestId('markdown-code-fade')).toBeNull();
  });
});
