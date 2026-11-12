import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import { avatarPackOf, type AvatarPack } from '@elitea/client-core/conversation';

import { FIXTURE_ORIGIN } from '@/api/__fixtures__/nativeClient';
import { PackAvatar } from '@/features/chat/AssistantAvatar';
import type { AgentMediaFiles } from '@/features/chat/agentMediaFiles';
import {
  MAX_AGENT_MEDIA_BYTES,
  avatarPacksById,
  fetchAgentMedia,
  packMediaPlan,
  participantAvatarPack,
  type AgentMedia,
} from '@/features/chat/avatarPackMedia';
import { INITIAL_SESSION, setSession } from '@/state/session';

// Issues 1835, 1864, 1882, 1883 (ADR-0050 decision 6): an agent's avatar pack in the mobile chat.

const mockExpoFetch = jest.fn();
jest.mock('expo/fetch', () => ({ fetch: (...args: unknown[]) => mockExpoFetch(...args) }));
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const media = (ext: string, digit = 'a') => `/api/v2/agent_media/7/${digit.repeat(64)}.${ext}`;
const STILL = media('png', 'a');
const THINKING = media('png', 'b');
const SPEAKING_WEBP = media('webp', 'c');
const WORKING_MP4 = media('mp4', 'd');
const WORKING_POSTER = media('jpg', 'e');
const SVG_STILL = media('svg', 'f');
const WAKING_WEBM = media('webm', '1');

const pack = (slots: Record<string, unknown>): AvatarPack => {
  const usable = avatarPackOf({ schema: 'elitea.avatar_pack/v1', slots });
  if (usable === undefined) throw new Error('not a usable pack');
  return usable;
};
/** A pack as the wire types it (the generated `AgentAvatarPackV1`). */
const wire = (value: unknown) => value as never;
const FULL = pack({
  still: { url: STILL },
  thinking: { url: THINKING },
  speaking: { url: SPEAKING_WEBP },
  working: { url: WORKING_MP4, poster_url: WORKING_POSTER },
  waking: { url: WAKING_WEBM, poster_url: STILL },
});

const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d]);
const ANIMATED_WEBP = new Uint8Array([0x52, 0x49, 0x46, 0x46, 4, 0, 0, 0, 0x57, 0x45, 0x42, 0x50, 0x56, 0x50, 0x38, 0x58, 10, 0, 0, 0, 0x02, 0]);
const MP4 = new Uint8Array([0, 0, 0, 0x18, 0x66, 0x74, 0x79, 0x70, 0x69, 0x73, 0x6f, 0x6d]);
const WEBM = new Uint8Array([0x1a, 0x45, 0xdf, 0xa3, 0x9f, 0x42, 0x86, 0x81]);

const answer = (bytes: Uint8Array, type = 'image/png', status = 200) =>
  new Response(bytes as unknown as BodyInit, { status, headers: { 'Content-Type': type } });

/** An in-memory stand-in for the caches folder. */
function memoryFiles(): AgentMediaFiles & { written: Map<string, Uint8Array> } {
  const written = new Map<string, Uint8Array>();
  const uriOf = (uri: string) => `file:///cache/agent-media/${uri.slice(uri.lastIndexOf('/') + 1)}`;
  return {
    written,
    cached: (uri) => (written.has(uriOf(uri)) ? uriOf(uri) : null),
    store: (uri, bytes) => {
      written.set(uriOf(uri), bytes);
      return uriOf(uri);
    },
  };
}

beforeEach(() => {
  mockExpoFetch.mockReset();
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('packMediaPlan', () => {
  it('shows the still for the still and for a state with no slot, with the default motion', () => {
    expect(packMediaPlan(FULL, 'still')).toEqual({ path: STILL, kind: 'image', ownMotion: false });
    expect(packMediaPlan(FULL, 'waiting')).toEqual({ path: STILL, kind: 'image', ownMotion: false });
  });

  it("shows a state's own image or animated WebP with no default motion", () => {
    expect(packMediaPlan(FULL, 'thinking')).toEqual({ path: THINKING, kind: 'image', ownMotion: true });
    expect(packMediaPlan(FULL, 'speaking')).toEqual({ path: SPEAKING_WEBP, kind: 'image', ownMotion: true });
  });

  it('plays a video slot over its poster, with no default motion', () => {
    expect(packMediaPlan(FULL, 'working')).toEqual({ path: WORKING_MP4, kind: 'video', poster: WORKING_POSTER, ownMotion: true });
    expect(packMediaPlan(FULL, 'waking')).toEqual({ path: WAKING_WEBM, kind: 'video', poster: STILL, ownMotion: true });
  });

  it('draws no SVG: an SVG still leaves the old icon, an SVG slot the still', () => {
    expect(packMediaPlan(pack({ still: { url: SVG_STILL } }), 'still')).toBeNull();
    expect(packMediaPlan(pack({ still: { url: STILL }, error: { url: SVG_STILL } }), 'error')).toEqual({ path: STILL, kind: 'image', ownMotion: false });
  });

  it('answers nothing for no pack', () => {
    expect(packMediaPlan(undefined, 'still')).toBeNull();
  });
});

describe('participantAvatarPack', () => {
  it('reads persona.avatar_pack_v1 (contract 1.49) first, then the object 1.46 to 1.48 sent as persona.avatar_pack', () => {
    const other = pack({ still: { url: THINKING } });
    expect(participantAvatarPack({ id: 3, entity_name: 'application', persona: { avatar_pack_v1: wire(FULL) } })?.slots.still?.url).toBe(STILL);
    const legacy = { id: 3, entity_name: 'application', persona: { avatar_pack: FULL as unknown as string } };
    expect(participantAvatarPack(legacy)?.slots.still?.url).toBe(STILL);
    const both = { id: 3, entity_name: 'application', persona: { avatar_pack_v1: wire(FULL), avatar_pack: other as unknown as string } };
    expect(participantAvatarPack(both)?.slots.still?.url).toBe(STILL);
    expect(participantAvatarPack({ id: 3, entity_name: 'application' })).toBeUndefined();
    expect(participantAvatarPack(undefined)).toBeUndefined();
  });

  it('rejects the string persona.avatar_pack a 1.38 to 1.45 server reserves, so such a server shows no pack', () => {
    const reserved = { id: 3, entity_name: 'application', persona: { avatar_pack: JSON.stringify(FULL) } };
    expect(participantAvatarPack(reserved)).toBeUndefined();
    expect(avatarPackOf(JSON.stringify(FULL))).toBeUndefined();
  });

  it('ignores media from anywhere else and an unknown schema', () => {
    const foreign = { schema: 'elitea.avatar_pack/v1', slots: { still: { url: 'https://evil.example/x.png' } } };
    const v2 = { schema: 'elitea.avatar_pack/v2', slots: { still: { url: STILL } } };
    for (const value of [foreign, v2, '{"schema":"elitea.avatar_pack/v1"}']) {
      expect(participantAvatarPack({ id: 3, entity_name: 'application', persona: { avatar_pack_v1: wire(value) } })).toBeUndefined();
    }
  });

  it('validates each participant once into a map by id (issue 1883)', () => {
    const packs = avatarPacksById([
      { id: 3, entity_name: 'application', persona: { avatar_pack_v1: wire(FULL) } },
      { id: 4, entity_name: 'application' },
      { id: 5, entity_name: 'user' },
    ]);
    expect([...packs.keys()]).toEqual([3]);
    expect(packs.get(3)?.slots.thinking?.url).toBe(THINKING);
    expect(avatarPacksById(undefined).size).toBe(0);
  });
});

describe('fetchAgentMedia', () => {
  it('downloads with redirects refused and no credential, and stores the checked bytes as a file', async () => {
    mockExpoFetch.mockResolvedValue(answer(PNG));
    const files = memoryFiles();
    const got = await fetchAgentMedia(`${FIXTURE_ORIGIN}${STILL}`, undefined, files);
    expect(got).toEqual({ kind: 'image', uri: `file:///cache/agent-media/${'a'.repeat(64)}.png` });
    expect(files.written.get(got!.uri)).toEqual(PNG);
    const [, init] = mockExpoFetch.mock.calls[0] as [string, RequestInit];
    expect(init.redirect).toBe('error');
    expect(new Headers(init.headers).get('Authorization')).toBeNull();
  });

  it('uses a file already on disk without a download: the name is the content address', async () => {
    const files = memoryFiles();
    files.store(`${FIXTURE_ORIGIN}${STILL}`, PNG);
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${STILL}`, undefined, files)).toEqual({ kind: 'image', uri: expect.stringMatching(/^file:/) });
    expect(mockExpoFetch).not.toHaveBeenCalled();
  });

  it('keeps an animated WebP, which expo-image plays, and stores an MP4 and a WebM as video files', async () => {
    const files = memoryFiles();
    mockExpoFetch.mockResolvedValueOnce(answer(ANIMATED_WEBP, 'image/webp'));
    expect((await fetchAgentMedia(`${FIXTURE_ORIGIN}${SPEAKING_WEBP}`, undefined, files))?.kind).toBe('image');
    mockExpoFetch.mockResolvedValueOnce(answer(MP4, 'video/mp4'));
    expect((await fetchAgentMedia(`${FIXTURE_ORIGIN}${WORKING_MP4}`, undefined, files))?.kind).toBe('video');
    mockExpoFetch.mockResolvedValueOnce(answer(WEBM, 'video/webm'));
    expect((await fetchAgentMedia(`${FIXTURE_ORIGIN}${WAKING_WEBM}`, undefined, files))?.kind).toBe('video');
  });

  it('draws an image from a data: URI when it cannot be stored, and drops a video', async () => {
    const unwritable: AgentMediaFiles = { cached: () => null, store: () => null };
    mockExpoFetch.mockResolvedValueOnce(answer(PNG));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${STILL}`, undefined, unwritable)).toEqual({
      kind: 'image',
      uri: `data:image/png;base64,${btoa(String.fromCharCode(...PNG))}`,
    });
    mockExpoFetch.mockResolvedValueOnce(answer(MP4, 'video/mp4'));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${WORKING_MP4}`, undefined, unwritable)).toBeNull();
  });

  it("refuses bytes or a Content-Type that is not the extension's type, an SVG, a failed status and an oversized file", async () => {
    const files = memoryFiles();
    mockExpoFetch.mockResolvedValueOnce(answer(PNG, 'video/mp4'));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${WORKING_MP4}`, undefined, files)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(MP4, 'image/png'));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${STILL}`, undefined, files)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(PNG, 'image/webp'));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${SPEAKING_WEBP}`, undefined, files)).toBeNull();
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${SVG_STILL}`, undefined, files)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(new TextEncoder().encode('<html>'), 'image/png'));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${STILL}`, undefined, files)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(PNG, 'image/png', 404));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${STILL}`, undefined, files)).toBeNull();
    const big = new Uint8Array(MAX_AGENT_MEDIA_BYTES + 1);
    big.set(PNG);
    mockExpoFetch.mockResolvedValueOnce(answer(big));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${STILL}`, undefined, files)).toBeNull();
    expect(files.written.size).toBe(0);
  });
});

describe('fetchAgentMedia in flight', () => {
  it('downloads one content address once while two callers ask at the same time', async () => {
    const files = memoryFiles();
    mockExpoFetch.mockReset();
    mockExpoFetch.mockResolvedValue(answer(PNG));
    const uri = `${FIXTURE_ORIGIN}${STILL}`;
    const [first, second] = await Promise.all([fetchAgentMedia(uri, undefined, files), fetchAgentMedia(uri, undefined, files)]);
    expect(first).not.toBeNull();
    expect(second).toEqual(first);
    expect(mockExpoFetch).toHaveBeenCalledTimes(1);
    expect(files.written.size).toBe(1);
  });
});

describe('PackAvatar', () => {
  const uriOf = (path: string) => `file:///cache/${path.slice(-8)}`;
  const served = (refuse: string[] = []) =>
    jest.fn(async (uri: string): Promise<AgentMedia | null> => {
      const path = new URL(uri).pathname;
      if (refuse.includes(path)) return null;
      return { kind: /\.(mp4|webm)$/.test(path) ? 'video' : 'image', uri: uriOf(path) };
    });
  const withQueries = (ui: ReactElement) =>
    render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{ui}</QueryClientProvider>);
  const image = () => screen.queryByTestId('avatar-image', { includeHiddenElements: true });
  const shown = () => image()?.props.source?.uri ?? null;
  const video = () => screen.queryByTestId('avatar-video', { includeHiddenElements: true });
  const poster = () => screen.queryByTestId('avatar-poster', { includeHiddenElements: true })?.props.source?.uri ?? null;
  const motionLayer = (state: string) => screen.queryByTestId(`agent-motion-${state}`, { includeHiddenElements: true });

  beforeEach(() => setSession({ ...INITIAL_SESSION, hydrated: true, serverOrigin: FIXTURE_ORIGIN, projectId: 7 }));
  afterEach(() => setSession({ ...INITIAL_SESSION }));

  it("shows the pack's still, not playing, in place of the old icon, and downloads nothing else", async () => {
    const fetchMedia = served();
    await withQueries(<PackAvatar projectId={7} pack={FULL} motion="still" name="Mira" imageUri="data:image/png;base64,OLDICON" mark fetchMedia={fetchMedia} />);
    await waitFor(() => expect(shown()).toBe(uriOf(STILL)));
    expect(image()?.props.autoplay).toBe(false);
    expect(fetchMedia).toHaveBeenCalledTimes(1);
  });

  it("plays a state's own image with no default motion over it", async () => {
    await withQueries(<PackAvatar projectId={7} pack={FULL} motion="thinking" name="Mira" imageUri={null} mark fetchMedia={served()} />);
    await waitFor(() => expect(shown()).toBe(uriOf(THINKING)));
    expect(motionLayer('thinking')).toBeNull();
  });

  it('plays an animated WebP slot with expo-image, with no default motion (issue 1864)', async () => {
    await withQueries(<PackAvatar projectId={7} pack={FULL} motion="speaking" name="Mira" imageUri={null} mark fetchMedia={served()} />);
    await waitFor(() => expect(shown()).toBe(uriOf(SPEAKING_WEBP)));
    expect(image()?.props.autoplay).toBe(true);
    expect(motionLayer('speaking')).toBeNull();
  });

  it('plays a video slot muted and looping over its poster, with no default motion (issue 1864)', async () => {
    await withQueries(<PackAvatar projectId={7} pack={FULL} motion="working" name="Mira" imageUri={null} mark fetchMedia={served()} />);
    await waitFor(() => expect(video()).toBeTruthy());
    const player = video()?.props.player as { source: unknown; loop: boolean; muted: boolean; playing: boolean; keepScreenOnWhilePlaying: boolean };
    expect(player).toMatchObject({ source: uriOf(WORKING_MP4), loop: true, muted: true, playing: true, keepScreenOnWhilePlaying: false });
    expect(video()?.props.nativeControls).toBe(false);
    await waitFor(() => expect(poster()).toBe(uriOf(WORKING_POSTER)));
    expect(motionLayer('working')).toBeNull();
    // The poster leaves once the first frame is drawn.
    await act(async () => video()?.props.onFirstFrameRender());
    expect(poster()).toBeNull();
  });

  it("uses the still as a video's poster when the slot names it, and brings the poster back on a player error", async () => {
    await withQueries(<PackAvatar projectId={7} pack={FULL} motion="waking" name="Mira" imageUri={null} mark fetchMedia={served()} />);
    await waitFor(() => expect(poster()).toBe(uriOf(STILL)));
    await act(async () => video()?.props.onFirstFrameRender());
    expect(poster()).toBeNull();
    const player = video()?.props.player as { emit: (event: string, payload: unknown) => void };
    await act(async () => player.emit('statusChange', { status: 'error' }));
    expect(poster()).toBe(uriOf(STILL));
  });

  it("shows the next state's poster until that video's own first frame", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const at = (motion: 'working' | 'waking') => (
      <QueryClientProvider client={client}>
        <PackAvatar projectId={7} pack={FULL} motion={motion} name="Mira" imageUri={null} mark fetchMedia={served()} />
      </QueryClientProvider>
    );
    const view = await render(at('working'));
    await waitFor(() => expect(poster()).toBe(uriOf(WORKING_POSTER)));
    await act(async () => video()?.props.onFirstFrameRender());
    expect(poster()).toBeNull();
    await view.rerender(at('waking'));
    await waitFor(() => expect((video()?.props.player as { source: unknown }).source).toBe(uriOf(WAKING_WEBM)));
    expect(poster()).toBe(uriOf(STILL));
    await act(async () => video()?.props.onFirstFrameRender());
    // Back to a state whose files are already loaded: the switch is immediate, and still a new
    // player with its poster up.
    await view.rerender(at('working'));
    expect((video()?.props.player as { source: unknown }).source).toBe(uriOf(WORKING_MP4));
    expect(poster()).toBe(uriOf(WORKING_POSTER));
  });

  it('keeps the still and the default motion for a state with no slot, or one whose file is refused', async () => {
    const view = await withQueries(<PackAvatar projectId={7} pack={FULL} motion="waiting" name="Mira" imageUri={null} mark fetchMedia={served()} />);
    await waitFor(() => expect(shown()).toBe(uriOf(STILL)));
    expect(motionLayer('waiting')).toBeTruthy();
    await view.unmount();
    await withQueries(<PackAvatar projectId={7} pack={FULL} motion="working" name="Mira" imageUri={null} mark fetchMedia={served([WORKING_MP4])} />);
    await waitFor(() => expect(shown()).toBe(uriOf(STILL)));
    expect(video()).toBeNull();
    expect(motionLayer('working')).toBeTruthy();
  });

  it("plays the still's file, with no default motion, for a state whose own slot is that file", async () => {
    const same = pack({ still: { url: SPEAKING_WEBP }, speaking: { url: SPEAKING_WEBP } });
    const fetchMedia = served();
    await withQueries(<PackAvatar projectId={7} pack={same} motion="speaking" name="Mira" imageUri={null} mark fetchMedia={fetchMedia} />);
    await waitFor(() => expect(shown()).toBe(uriOf(SPEAKING_WEBP)));
    expect(image()?.props.autoplay).toBe(true);
    expect(motionLayer('speaking')).toBeNull();
    expect(fetchMedia).toHaveBeenCalledTimes(1);
  });

  it('fetches a kept file again when the view cannot load it (the OS evicted it), once', async () => {
    const fetchMedia = served();
    await withQueries(<PackAvatar projectId={7} pack={FULL} motion="still" name="Mira" imageUri={null} mark fetchMedia={fetchMedia} />);
    await waitFor(() => expect(shown()).toBe(uriOf(STILL)));
    expect(fetchMedia).toHaveBeenCalledTimes(1);
    fireEvent(image()!, 'error');
    await waitFor(() => expect(fetchMedia).toHaveBeenCalledTimes(2));
    // A second failure of the same file does not loop.
    fireEvent(image()!, 'error');
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(fetchMedia).toHaveBeenCalledTimes(2);
  });

  it('falls back to the old icon when the still is refused, and to the monogram without either', async () => {
    const view = await withQueries(
      <PackAvatar projectId={7} pack={FULL} motion="still" name="Mira" imageUri="data:image/png;base64,OLDICON" mark={false} fetchMedia={served([STILL])} />,
    );
    await waitFor(() => expect(shown()).toBe('data:image/png;base64,OLDICON'));
    await view.unmount();
    await withQueries(<PackAvatar projectId={7} pack={undefined} motion="still" name="Mira Quinn" imageUri={null} mark={false} fetchMedia={served()} />);
    expect(shown()).toBeNull();
    expect(screen.getByText('MQ', { includeHiddenElements: true })).toBeTruthy();
  });
});
