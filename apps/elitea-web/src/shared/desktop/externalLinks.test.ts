import { afterEach, describe, expect, it, vi } from 'vitest';

import { installExternalLinks } from './externalLinks';

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
  return { open };
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
});
