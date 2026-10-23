import { afterEach, describe, expect, it, vi } from 'vitest';

import { installExternalLinks, installExternalLinksIn } from './externalLinks';

let dispose: (() => void) | undefined;
afterEach(() => {
  dispose?.();
  document.body.innerHTML = '';
});

function setup() {
  const open = vi.fn<(url: string) => Promise<void>>().mockResolvedValue();
  const nativeOpen = vi.fn<typeof window.open>();
  window.open = nativeOpen;
  dispose = installExternalLinks({ openExternal: open }, 'https://elitea.example.com', document, window);
  return { open, nativeOpen };
}

function click(html: string): MouseEvent {
  document.body.innerHTML = html;
  const link = document.querySelector('a');
  const event = new MouseEvent('click', { bubbles: true, cancelable: true });
  link?.dispatchEvent(event);
  return event;
}

describe('installExternalLinks', () => {
  it('opens target=_blank http(s) links in the system browser and cancels the navigation', () => {
    const { open } = setup();
    const event = click('<a href="https://docs.example/x" target="_blank">d</a>');
    expect(open).toHaveBeenCalledWith('https://docs.example/x');
    expect(event.defaultPrevented).toBe(true);
  });

  it('never hands a non-http(s) scheme to the opener', () => {
    const { open } = setup();
    click('<a href="file:///etc/passwd" target="_blank">x</a>');
    click('<a href="javascript:alert(1)" target="_blank">x</a>');
    expect(open).not.toHaveBeenCalled();
  });

  it('resolves a relative _blank link against the deployment origin, not the bundled page', () => {
    const { open } = setup();
    click('<a href="/app/docs/guide" target="_blank">g</a>');
    expect(open).toHaveBeenCalledWith('https://elitea.example.com/app/docs/guide');
    window.open('files/report.pdf');
    expect(open).toHaveBeenLastCalledWith('https://elitea.example.com/files/report.pdf');
  });

  it('leaves in-app links alone', () => {
    const { open } = setup();
    const event = click('<a href="/chat">c</a>');
    expect(open).not.toHaveBeenCalled();
    expect(event.defaultPrevented).toBe(false);
  });

  it('routes window.open(http url) to the system browser and returns null', () => {
    const { open } = setup();
    expect(window.open('https://x.example/', '_blank')).toBeNull();
    expect(open).toHaveBeenCalledWith('https://x.example/');
    window.open('data:text/html,hi');
    expect(open).toHaveBeenCalledTimes(1);
  });

  it('lets the page open an inspector tab in its own (blank, same-origin) window, and nothing else', () => {
    const { open, nativeOpen } = setup();
    window.open('about:blank#elitea-panel=changes', 'elitea-panel-changes', 'popup');
    expect(nativeOpen).toHaveBeenCalledWith('about:blank#elitea-panel=changes', 'elitea-panel-changes', 'popup');
    window.open('about:blank#elitea-panel=../x');
    window.open('about:blank');
    expect(nativeOpen).toHaveBeenCalledTimes(1);
    expect(open).not.toHaveBeenCalled();
  });
});

describe('installExternalLinksIn', () => {
  it('handles a panel window\'s links like the main window\'s, until removed or uninstalled', () => {
    const { open } = setup();
    const frame = document.createElement('iframe');
    document.body.append(frame);
    const panel = frame.contentDocument as Document;
    const clickIn = (): void => {
      panel.body.innerHTML = '<a href="/docs" target="_blank">d</a>';
      panel.querySelector('a')?.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
    };
    const remove = installExternalLinksIn(panel);
    clickIn();
    expect(open).toHaveBeenCalledWith('https://elitea.example.com/docs');
    remove();
    clickIn();
    expect(open).toHaveBeenCalledTimes(1);

    // Without the desktop's handler there is nothing to install.
    dispose?.();
    dispose = undefined;
    installExternalLinksIn(panel);
    clickIn();
    expect(open).toHaveBeenCalledTimes(1);
  });
});
