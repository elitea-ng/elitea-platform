import { fireEvent, render, screen } from '@testing-library/react-native';
import { Image, Platform, StyleSheet, type TextStyle } from 'react-native';

import { MONOSPACE_FONT, Markdown } from '@/features/markdown/Markdown';
import { safeLinkHost } from '@/features/markdown/links';

jest.mock('expo-web-browser', () => ({ openBrowserAsync: jest.fn(async () => ({})) }));

describe('safeLinkHost', () => {
  it('accepts absolute https links and returns the host', () => {
    expect(safeLinkHost('https://docs.example.com/a?b=1', false)).toBe('docs.example.com');
    expect(safeLinkHost('HTTPS://Docs.Example.com:8443/x', false)).toBe('docs.example.com:8443');
  });

  it('refuses every other scheme, relative links and control characters', () => {
    for (const href of [
      'javascript:alert(1)',
      'JavaScript:alert(1)',
      'data:text/html,<b>x</b>',
      'file:///etc/passwd',
      'agentzefir://connect?server=x',
      '//evil.example/x',
      '/relative',
      'https://evil.example/\nx',
      'https://user@evil.example/',
      'http://plain.example/',
    ]) {
      expect(safeLinkHost(href, false)).toBeNull();
    }
    expect(safeLinkHost('http://localhost:3000/', true)).toBe('localhost:3000');
  });
});

describe('Markdown with the SafeRenderer', () => {
  it('renders an image as a chip and never mounts an Image', async () => {
    const openLink = jest.fn();
    const getSize = jest.spyOn(Image, 'getSize').mockImplementation(() => {});
    const prefetch = jest.spyOn(Image, 'prefetch').mockImplementation(async () => true);
    await render(
      <Markdown value={'Look: ![secret chart](https://evil.example/leak.png?data=abc)'} openLink={openLink} />,
    );
    const chip = screen.getByTestId('markdown-image-chip');
    expect(chip).toHaveTextContent('[secret chart · evil.example]');
    // No element anywhere carries the image URL (no <Image source>), and nothing measured or fetched it.
    expect(JSON.stringify(screen.toJSON())).not.toContain('leak.png');
    expect(getSize).not.toHaveBeenCalled();
    expect(prefetch).not.toHaveBeenCalled();
    await fireEvent.press(chip);
    expect(openLink).toHaveBeenCalledWith('https://evil.example/leak.png?data=abc', 'evil.example');
  });

  it('opens an https link only through the opener', async () => {
    const openLink = jest.fn();
    await render(<Markdown value={'See [the docs](https://docs.example.com/x).'} openLink={openLink} />);
    await fireEvent.press(screen.getByText('the docs'));
    expect(openLink).toHaveBeenCalledWith('https://docs.example.com/x', 'docs.example.com');
  });

  it('makes a javascript: link inert', async () => {
    const openLink = jest.fn();
    await render(<Markdown value={'[click me](javascript:alert(1))'} openLink={openLink} />);
    const node = screen.getByText('click me');
    expect(node.props.onPress).toBeUndefined();
    await fireEvent.press(node);
    expect(openLink).not.toHaveBeenCalled();
  });

  it('keeps raw HTML as text', async () => {
    await render(<Markdown value={'<img src="https://evil.example/x.png"> <b>bold?</b>'} openLink={jest.fn()} />);
    expect(screen.getByText(/<img src="https:\/\/evil.example\/x.png">/)).toBeTruthy();
  });

  it('renders ordinary markdown', async () => {
    await render(<Markdown value={'# Title\n\nSome **bold** text.\n\n- one\n- two'} openLink={jest.fn()} />);
    expect(screen.getByText('Title')).toBeTruthy();
    expect(screen.getByText('bold')).toBeTruthy();
    expect(screen.getByText('two')).toBeTruthy();
  });

  // DEF-6 (Agent Zefir E2E): inline code rendered in italic proportional type
  // (react-native-marked's default codespan style), not monospace.
  it('renders inline and block code in an upright monospace face', async () => {
    await render(<Markdown value={'Hello **bold** and `code`\n\n```\nblock()\n```'} openLink={jest.fn()} />);
    for (const text of ['code', 'block()']) {
      const style = StyleSheet.flatten(screen.getByText(text).props.style) as TextStyle;
      expect(style.fontFamily).toBe(MONOSPACE_FONT);
      expect(style.fontStyle ?? 'normal').toBe('normal');
    }
    expect(MONOSPACE_FONT).toBe(Platform.select({ ios: 'Menlo', default: 'monospace' }));
  });
});
