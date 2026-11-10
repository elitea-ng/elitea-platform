import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import { FIXTURE_ORIGIN } from '@/api/__fixtures__/nativeClient';
import { PackAvatar } from '@/features/chat/AssistantAvatar';
import {
  MAX_AGENT_MEDIA_BYTES,
  fetchAgentMedia,
  isAnimatedWebp,
  packMediaPlan,
  participantAvatarPack,
  type AgentMedia,
} from '@/features/chat/avatarPackMedia';
import { INITIAL_SESSION, setSession } from '@/state/session';

// Issue 1835 (ADR-0050 decision 6): an agent's avatar pack in the mobile chat.

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

const pack = (slots: Record<string, unknown>) => ({ schema: 'elitea.avatar_pack/v1', slots });
const FULL = pack({
  still: { url: STILL },
  thinking: { url: THINKING },
  speaking: { url: SPEAKING_WEBP },
  working: { url: WORKING_MP4, poster_url: WORKING_POSTER },
});

const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d]);
const STATIC_WEBP = new Uint8Array([0x52, 0x49, 0x46, 0x46, 4, 0, 0, 0, 0x57, 0x45, 0x42, 0x50, 0x56, 0x50, 0x38, 0x20, 0, 0, 0, 0, 0, 0]);
const ANIMATED_WEBP = new Uint8Array([0x52, 0x49, 0x46, 0x46, 4, 0, 0, 0, 0x57, 0x45, 0x42, 0x50, 0x56, 0x50, 0x38, 0x58, 10, 0, 0, 0, 0x02, 0]);

const answer = (bytes: Uint8Array, type = 'image/png', status = 200) =>
  new Response(bytes as unknown as BodyInit, { status, headers: { 'Content-Type': type } });

beforeEach(() => {
  mockExpoFetch.mockReset();
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('packMediaPlan', () => {
  it('shows the still for the still and for a state with no slot, with the default motion', () => {
    expect(packMediaPlan(FULL, 'still')).toEqual({ path: STILL, ownMotion: false });
    expect(packMediaPlan(FULL, 'waiting')).toEqual({ path: STILL, ownMotion: false });
  });

  it("shows a state's own image with no default motion", () => {
    expect(packMediaPlan(FULL, 'thinking')).toEqual({ path: THINKING, ownMotion: true });
    expect(packMediaPlan(FULL, 'speaking')).toEqual({ path: SPEAKING_WEBP, ownMotion: true });
  });

  it("shows a video slot's poster with the default motion: this build has no video component", () => {
    expect(packMediaPlan(FULL, 'working')).toEqual({ path: WORKING_POSTER, ownMotion: false });
  });

  it('draws no SVG: an SVG still leaves the old icon, an SVG slot the still', () => {
    expect(packMediaPlan(pack({ still: { url: SVG_STILL } }), 'still')).toBeNull();
    expect(packMediaPlan(pack({ still: { url: STILL }, error: { url: SVG_STILL } }), 'error')).toEqual({ path: STILL, ownMotion: false });
  });

  it('answers nothing for no pack, an unknown schema, or media from anywhere else', () => {
    expect(packMediaPlan(undefined, 'still')).toBeNull();
    expect(packMediaPlan('{"schema":"elitea.avatar_pack/v1"}', 'still')).toBeNull();
    expect(packMediaPlan({ schema: 'elitea.avatar_pack/v2', slots: { still: { url: STILL } } }, 'still')).toBeNull();
    expect(packMediaPlan(pack({ still: { url: 'https://evil.example/x.png' } }), 'still')).toBeNull();
  });
});

describe('participantAvatarPack', () => {
  it('reads persona.avatar_pack, typed as a string in the lock but sent as an object', () => {
    const participant = { id: 3, entity_name: 'application', persona: { display_name: 'Mira', avatar_pack: FULL as unknown as string } };
    expect(participantAvatarPack(participant)?.slots.still?.url).toBe(STILL);
    expect(participantAvatarPack({ id: 3, entity_name: 'application' })).toBeUndefined();
    expect(participantAvatarPack(undefined)).toBeUndefined();
  });
});

describe('fetchAgentMedia', () => {
  it('downloads with redirects refused and no credential, and answers the checked bytes', async () => {
    mockExpoFetch.mockResolvedValue(answer(PNG));
    const got = await fetchAgentMedia(`${FIXTURE_ORIGIN}${STILL}`);
    expect(got).toEqual({ kind: 'image', uri: `data:image/png;base64,${btoa(String.fromCharCode(...PNG))}` });
    const [, init] = mockExpoFetch.mock.calls[0] as [string, RequestInit];
    expect(init.redirect).toBe('error');
    expect(new Headers(init.headers).get('Authorization')).toBeNull();
  });

  it('keeps a static WebP and drops the bytes of an animated one, which this build cannot play', async () => {
    expect(isAnimatedWebp(ANIMATED_WEBP)).toBe(true);
    expect(isAnimatedWebp(STATIC_WEBP)).toBe(false);
    mockExpoFetch.mockResolvedValueOnce(answer(STATIC_WEBP, 'image/webp'));
    expect((await fetchAgentMedia(`${FIXTURE_ORIGIN}${SPEAKING_WEBP}`))?.kind).toBe('image');
    mockExpoFetch.mockResolvedValueOnce(answer(ANIMATED_WEBP, 'image/webp'));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${SPEAKING_WEBP}`)).toEqual({ kind: 'animated' });
  });

  it('keeps an animated WebP still, drawn as its first frame: the server accepts one as the still', async () => {
    mockExpoFetch.mockResolvedValueOnce(answer(ANIMATED_WEBP, 'image/webp'));
    expect((await fetchAgentMedia(`${FIXTURE_ORIGIN}${SPEAKING_WEBP}`, { still: true }))?.kind).toBe('image');
  });

  it('refuses a video, an SVG, a non-image body, a failed status and an oversized file', async () => {
    mockExpoFetch.mockResolvedValueOnce(answer(PNG, 'video/mp4'));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${WORKING_MP4}`)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(new TextEncoder().encode('<svg/>'), 'image/svg+xml'));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${SVG_STILL}`)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(new TextEncoder().encode('<html>'), 'image/png'));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${STILL}`)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(PNG, 'image/png', 404));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${STILL}`)).toBeNull();
    const big = new Uint8Array(MAX_AGENT_MEDIA_BYTES + 1);
    big.set(PNG);
    mockExpoFetch.mockResolvedValueOnce(answer(big));
    expect(await fetchAgentMedia(`${FIXTURE_ORIGIN}${STILL}`)).toBeNull();
  });
});

describe('PackAvatar', () => {
  const uriOf = (path: string) => `data:image/png;base64,${path.slice(-8)}`;
  const served = (refuse: string[] = [], animated: string[] = []) =>
    jest.fn(async (uri: string): Promise<AgentMedia | null> => {
      const path = new URL(uri).pathname;
      if (refuse.includes(path)) return null;
      if (animated.includes(path)) return { kind: 'animated' };
      return { kind: 'image', uri: uriOf(path) };
    });
  const withQueries = (ui: ReactElement) =>
    render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{ui}</QueryClientProvider>);
  const shown = () => screen.queryByTestId('avatar-image', { includeHiddenElements: true })?.props.source?.uri ?? null;
  const motionLayer = (state: string) => screen.queryByTestId(`agent-motion-${state}`, { includeHiddenElements: true });

  beforeEach(() => setSession({ ...INITIAL_SESSION, hydrated: true, serverOrigin: FIXTURE_ORIGIN, projectId: 7 }));
  afterEach(() => setSession({ ...INITIAL_SESSION }));

  it("shows the pack's still in place of the old icon, and downloads nothing else", async () => {
    const fetchMedia = served();
    await withQueries(<PackAvatar projectId={7} pack={FULL} motion="still" name="Mira" imageUri="data:image/png;base64,OLDICON" mark fetchMedia={fetchMedia} />);
    await waitFor(() => expect(shown()).toBe(uriOf(STILL)));
    expect(fetchMedia).toHaveBeenCalledTimes(1);
  });

  it("plays a state's own image with no default motion over it", async () => {
    await withQueries(<PackAvatar projectId={7} pack={FULL} motion="thinking" name="Mira" imageUri={null} mark fetchMedia={served()} />);
    await waitFor(() => expect(shown()).toBe(uriOf(THINKING)));
    expect(motionLayer('thinking')).toBeNull();
  });

  it('keeps the still and the default motion for a state with no slot', async () => {
    await withQueries(<PackAvatar projectId={7} pack={FULL} motion="waiting" name="Mira" imageUri={null} mark fetchMedia={served()} />);
    await waitFor(() => expect(shown()).toBe(uriOf(STILL)));
    expect(motionLayer('waiting')).toBeTruthy();
  });

  it('shows the poster of a video slot, with the default motion', async () => {
    await withQueries(<PackAvatar projectId={7} pack={FULL} motion="working" name="Mira" imageUri={null} mark fetchMedia={served()} />);
    await waitFor(() => expect(shown()).toBe(uriOf(WORKING_POSTER)));
    expect(motionLayer('working')).toBeTruthy();
  });

  it('shows the still with the default motion for an animated WebP this build cannot play', async () => {
    await withQueries(
      <PackAvatar projectId={7} pack={FULL} motion="speaking" name="Mira" imageUri={null} mark fetchMedia={served([], [SPEAKING_WEBP])} />,
    );
    await waitFor(() => expect(shown()).toBe(uriOf(STILL)));
    expect(motionLayer('speaking')).toBeTruthy();
  });

  it('asks for the still as a still, so an animated WebP still is kept', async () => {
    const fetchMedia = served();
    await withQueries(<PackAvatar projectId={7} pack={FULL} motion="still" name="Mira" imageUri={null} mark fetchMedia={fetchMedia} />);
    await waitFor(() => expect(shown()).toBe(uriOf(STILL)));
    expect(fetchMedia).toHaveBeenCalledWith(`${FIXTURE_ORIGIN}${STILL}`, { still: true });
  });

  it("draws no default motion for a state whose own slot is the still's file", async () => {
    const same = pack({ still: { url: STILL }, speaking: { url: STILL } });
    const fetchMedia = served();
    await withQueries(<PackAvatar projectId={7} pack={same} motion="speaking" name="Mira" imageUri={null} mark fetchMedia={fetchMedia} />);
    await waitFor(() => expect(shown()).toBe(uriOf(STILL)));
    expect(motionLayer('speaking')).toBeNull();
    expect(fetchMedia).toHaveBeenCalledTimes(1);
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
