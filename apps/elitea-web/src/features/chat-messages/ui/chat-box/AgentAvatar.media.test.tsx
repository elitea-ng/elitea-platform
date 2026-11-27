/**
 * #3132: an avatar pack's files (and the icon) are server paths. The desktop app's page is not on the deployment's
 * origin and loads media only from itself, `data:` and `blob:`, so it fetches them through its host and shows
 * `blob:` URLs; the web loads the path, on the deployment's origin when that is another one.
 */
import { screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { setNativeTransport } from '@/shared/api/nativeTransport';
import { resetConfigForTests } from '@/shared/config/get-config';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { resetAgentMediaForTests } from '../../lib/agentMediaSrc';
import { AgentAvatar } from './AgentAvatar';

const globals = globalThis as unknown as Record<string, unknown>;
const media = (sha: string, ext: string) => `/api/v2/agent_media/1/${sha.repeat(32)}.${ext}`;
const STILL = media('ab', 'png');
const LOOP = media('cd', 'mp4');
const pack = { schema: 'elitea.avatar_pack/v1', slots: { still: { url: STILL }, working: { url: LOOP, poster_url: STILL } } };

function serve(origin: string): void {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: `${origin}/api/v2`, vite_base_uri: '/', vite_public_project_id: '1' };
}

beforeEach(() => {
  resetAgentMediaForTests();
  vi.stubGlobal('URL', Object.assign(class extends URL {}, { createObjectURL: (blob: Blob) => `blob:app/${String(blob.size)}-${blob.type}`, revokeObjectURL: () => undefined }));
});

afterEach(() => {
  setNativeTransport(undefined);
  vi.unstubAllGlobals();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

describe('AgentAvatar media (#3132)', () => {
  it('desktop: plays a video slot as a muted looping <video> from a blob: URL fetched through the host, once per file', async () => {
    serve('https://elitea.example');
    const fetched: string[] = [];
    const hostFetch = vi.fn((input: string) => {
      const url = input;
      fetched.push(url);
      const type = url.endsWith('.mp4') ? 'video/mp4' : 'image/png';
      return Promise.resolve(new Response(new Blob([url.endsWith('.mp4') ? 'loop' : 'still'], { type }), { status: 200, headers: { 'Content-Type': type } }));
    });
    setNativeTransport({ accessToken: () => Promise.resolve('t'), refresh: () => Promise.resolve('refreshed'), signOut: () => undefined, origin: 'https://elitea.example', fetch: hostFetch as unknown as typeof fetch });

    renderWithTheme(
      <>
        <AgentAvatar pack={pack} motion="working" />
        <AgentAvatar pack={pack} motion="working" />
      </>,
    );
    const videos = await screen.findAllByTestId('agent-avatar-video');
    expect(videos).toHaveLength(2);
    const video = videos[0] as HTMLVideoElement;
    expect(video.tagName).toBe('VIDEO');
    expect(video.getAttribute('src')).toMatch(/^blob:app\/\d+-video\/mp4$/u);
    await waitFor(() => expect(video.getAttribute('poster')).toMatch(/^blob:app\/\d+-image\/png$/u));
    expect(video.muted).toBe(true);
    expect(video.loop).toBe(true);
    expect(video.autoplay).toBe(true);
    // On the deployment's origin, each file once for both avatars.
    expect([...fetched].sort()).toEqual([`https://elitea.example${STILL}`, `https://elitea.example${LOOP}`]);
  });

  it('desktop: shows the fallback, never a broken image, while a file has not loaded or could not be fetched', async () => {
    serve('https://elitea.example');
    const hostFetch = vi.fn(() => Promise.resolve(new Response('missing', { status: 404 })));
    setNativeTransport({ accessToken: () => Promise.resolve('t'), refresh: () => Promise.resolve('refreshed'), signOut: () => undefined, origin: 'https://elitea.example', fetch: hostFetch as unknown as typeof fetch });

    const { container } = renderWithTheme(<AgentAvatar pack={pack} motion="still" />);
    await waitFor(() => expect(hostFetch).toHaveBeenCalled());
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector('svg')).not.toBeNull();
  });

  it('web: loads the path as it is on the page\'s own origin, and on the deployment\'s when it is another', () => {
    serve(window.location.origin);
    const { container, unmount } = renderWithTheme(<AgentAvatar pack={pack} motion="still" />);
    expect(container.querySelector('img')?.getAttribute('src')).toBe(STILL);
    unmount();

    serve('https://api.elitea.example');
    const other = renderWithTheme(<AgentAvatar pack={pack} motion="working" />);
    expect(screen.getByTestId('agent-avatar-video').getAttribute('src')).toBe(`https://api.elitea.example${LOOP}`);
    expect(other.container.querySelector('video')?.getAttribute('poster')).toBe(`https://api.elitea.example${STILL}`);
  });
});
