/**
 * ADR-0050 decision 6 (issue 1619) — the Avatar tab, exercised through the
 * REAL agent editor route: every state has a preview card, a stored pack
 * fills its slots and the empty states show the default motion over the
 * still, an upload fills a slot (a video takes the still as its poster), the
 * server's refusal is shown, and Save sends the edited pack.
 */
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { AgentAvatarPackV1 } from '@/shared/api/generated/model';
import { getGetApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { server } from '@/test/setup';

import { EditApplication } from './EditApplication';
import { renderAgentsRoute } from './__tests__/testRouter';

installCodeMirrorTestPolyfills();

const media = (hex: string, ext: string) => `/api/v2/agent_media/9/${hex.repeat(32)}.${ext}`;
const STILL = media('ab', 'png');
const THINKING = media('cd', 'webp');
const SPEAKING = media('ef', 'webm');
const STATES = ['idle', 'thinking', 'speaking', 'working', 'waiting', 'success', 'error', 'waking'] as const;

function detail(avatarPack: AgentAvatarPackV1 | null) {
  return {
    id: '42',
    name: 'My Agent',
    description: 'A helpful agent',
    icon: '',
    owner_id: 'user-1',
    created_at: '2026-01-01T00:00:00Z',
    versions: [{ id: '1', name: 'base', status: 'draft', agent_type: 'classic', created_at: '2026-01-01T00:00:00Z' }],
    version_details: {
      id: '1',
      application_id: '42',
      name: 'base',
      status: 'draft',
      instructions: 'Be helpful.',
      conversation_starters: [],
      avatar_pack: avatarPack,
    },
  };
}

const STORED_PACK: AgentAvatarPackV1 = {
  schema: 'elitea.avatar_pack/v1',
  slots: { still: { url: STILL, media_type: 'image/png' }, thinking: { url: THINKING, media_type: 'image/webp' } },
};

function captureVersionSave(sink: Record<string, unknown>[]): void {
  server.use(
    http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
      sink.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json({ id: '1', application_id: '42', name: 'base', status: 'draft' }, { status: 201 });
    }),
    http.put('*/elitea_core/application/prompt_lib/:projectId/:id', () => HttpResponse.json({ id: '42' }, { status: 201 })),
  );
}

function mediaObject(url: string) {
  return { url, media_type: 'video/webm', format: 'webm', width: 256, height: 256, duration_ms: 2000, bytes: 4096 };
}

/** The project's models; `image_generation` answers `imageModels`, every other section the chat model. */
function serveModels(imageModels: readonly { name: string; display_name: string }[]): void {
  server.use(
    http.get('*/configurations/models/:projectId', ({ request }) =>
      new URL(request.url).searchParams.get('section') === 'image_generation'
        ? HttpResponse.json({ items: imageModels.map((model) => ({ ...model, project_id: '9' })), total: imageModels.length })
        : HttpResponse.json({
            items: [{ name: 'gpt-4o', display_name: 'GPT-4o', project_id: '9', default: true }],
            default_model_name: 'gpt-4o',
          }),
    ),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  serveModels([]);
  server.use(
    http.get('*/social/author*', () => HttpResponse.json({ id: '6', name: 'Driver', avatar: '' })),
  );
});

afterEach(() => resetGeneratedClient());

async function openAvatarTab(): Promise<ReturnType<typeof userEvent.setup>> {
  renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
  const user = userEvent.setup();
  await user.click(await screen.findByTestId('edit-application-tab-avatar', {}, { timeout: 5_000 }));
  await screen.findByTestId('edit-application-avatar-panel');
  return user;
}

describe('EditApplication — the Avatar tab', () => {
  it('previews the still and every state, with the stored slots and the default motion for the rest', async () => {
    server.use(getGetApplicationMockHandler(detail(STORED_PACK)));
    await openAvatarTab();

    const still = await screen.findByTestId('avatar-slot-still');
    await waitFor(() => expect(still.querySelector('img')?.getAttribute('src')).toBe(STILL));
    expect(within(still).getByTestId('chat-message-avatar')).toHaveAttribute('data-motion', 'still');

    for (const state of STATES) {
      const card = screen.getByTestId(`avatar-slot-${state}`);
      expect(within(card).getByTestId('chat-message-avatar')).toHaveAttribute('data-motion', state);
    }
    const thinking = screen.getByTestId('avatar-slot-thinking');
    expect(thinking.querySelector('img')?.getAttribute('src')).toBe(THINKING);
    expect(within(thinking).queryByTestId('agent-avatar-motion')).toBeNull();
    expect(screen.getByTestId('avatar-slot-thinking-source')).toHaveTextContent('Own file');

    const working = screen.getByTestId('avatar-slot-working');
    expect(working.querySelector('img')?.getAttribute('src')).toBe(STILL);
    expect(within(working).getByTestId('agent-avatar-motion')).toBeInTheDocument();
    expect(screen.getByTestId('avatar-slot-working-source')).toHaveTextContent('Default motion');
  }, 20_000);

  it('uploads a video into a state, with the still as its poster, and saves the pack', async () => {
    server.use(getGetApplicationMockHandler(detail(STORED_PACK)));
    const uploads: string[] = [];
    server.use(
      http.post('*/elitea_core/agent_media/prompt_lib/:projectId', ({ params }) => {
        uploads.push(String(params['projectId']));
        return HttpResponse.json(mediaObject(SPEAKING), { status: 201 });
      }),
    );
    const bodies: Record<string, unknown>[] = [];
    captureVersionSave(bodies);
    const user = await openAvatarTab();
    await waitFor(() => expect(screen.getByTestId('avatar-slot-still').querySelector('img')).not.toBeNull());

    await user.click(screen.getByTestId('avatar-slot-speaking-upload'));
    await user.upload(screen.getByTestId('avatar-file-input'), new File(['x'], 'talk.webm', { type: 'video/webm' }));

    const video = (await within(screen.getByTestId('avatar-slot-speaking')).findByTestId('agent-avatar-video')) as HTMLVideoElement;
    expect(video.getAttribute('src')).toBe(SPEAKING);
    expect(video.getAttribute('poster')).toBe(STILL);
    expect(video.muted).toBe(true);
    expect(uploads).toEqual(['9']);

    await waitFor(() => expect(screen.getByTestId('agent-save-button')).toBeEnabled());
    await user.click(screen.getByTestId('agent-save-button'));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]?.['avatar_pack']).toEqual({
      schema: 'elitea.avatar_pack/v1',
      slots: { still: { url: STILL }, thinking: { url: THINKING }, speaking: { url: SPEAKING, poster_url: STILL } },
    });
  }, 20_000);

  it("shows the server's refusal and leaves the slot empty", async () => {
    server.use(getGetApplicationMockHandler(detail(STORED_PACK)));
    server.use(
      http.post('*/elitea_core/agent_media/prompt_lib/:projectId', () =>
        HttpResponse.json({ error: 'the video has an audio track; an avatar animation must be muted', code: 'avatar_media_invalid' }, { status: 400 }),
      ),
    );
    const user = await openAvatarTab();
    await waitFor(() => expect(screen.getByTestId('avatar-slot-still').querySelector('img')).not.toBeNull());

    await user.click(screen.getByTestId('avatar-slot-waking-upload'));
    await user.upload(screen.getByTestId('avatar-file-input'), new File(['x'], 'loud.mp4', { type: 'video/mp4' }));

    expect(await screen.findByTestId('avatar-problem')).toHaveTextContent('must be muted');
    expect(screen.getByTestId('avatar-slot-waking')).toHaveAttribute('data-filled', 'false');
  }, 20_000);

  it('refuses a video for a state when the still is an SVG, which the server takes as no poster', async () => {
    const svgStill = media('ab', 'svg');
    server.use(
      getGetApplicationMockHandler(
        detail({ schema: 'elitea.avatar_pack/v1', slots: { still: { url: svgStill, media_type: 'image/svg+xml' } } }),
      ),
    );
    server.use(http.post('*/elitea_core/agent_media/prompt_lib/:projectId', () => HttpResponse.json(mediaObject(SPEAKING), { status: 201 })));
    const user = await openAvatarTab();
    await waitFor(() => expect(screen.getByTestId('avatar-slot-still').querySelector('img')).not.toBeNull());

    await user.click(screen.getByTestId('avatar-slot-speaking-upload'));
    await user.upload(screen.getByTestId('avatar-file-input'), new File(['x'], 'talk.webm', { type: 'video/webm' }));

    expect(await screen.findByTestId('avatar-problem')).toHaveTextContent('PNG, JPEG or WebP still');
    expect(screen.getByTestId('avatar-slot-speaking')).toHaveAttribute('data-filled', 'false');
  }, 20_000);

  it('asks for a still before a state, when the version has no pack', async () => {
    server.use(getGetApplicationMockHandler(detail(null)));
    const user = await openAvatarTab();

    expect(screen.getByTestId('avatar-slot-still-source')).toHaveTextContent('No still');
    await user.click(screen.getByTestId('avatar-slot-idle-upload'));
    expect(screen.getByTestId('avatar-problem')).toHaveTextContent('Add a still first');
    expect(screen.queryByTestId('avatar-generate')).toBeNull();
  }, 20_000);

  it('generates still candidates from the persona with the image model, and a chosen one fills the still', async () => {
    serveModels([{ name: 'gpt-image-1', display_name: 'GPT Image' }]);
    server.use(getGetApplicationMockHandler({ ...detail(null), version_details: { ...detail(null).version_details, soul: { schema: 'elitea.soul/v1', display_name: 'Ada', role: 'Reviewer' } } }));
    const requests: Record<string, unknown>[] = [];
    server.use(
      http.post('*/elitea_core/agent_media_generate/prompt_lib/:projectId', async ({ request }) => {
        requests.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ media: [{ ...mediaObject(STILL), media_type: 'image/png', format: 'png', duration_ms: 0 }] }, { status: 201 });
      }),
    );
    const user = await openAvatarTab();

    await user.click(await screen.findByTestId('avatar-generate-stills', {}, { timeout: 5_000 }));
    await user.click(await screen.findByTestId('avatar-candidate'));

    expect(requests[0]).toMatchObject({ kind: 'still', model: 'gpt-image-1', display_name: 'Ada', role: 'Reviewer', count: 4 });
    await waitFor(() => expect(screen.getByTestId('avatar-slot-still').querySelector('img')?.getAttribute('src')).toBe(STILL));
    expect(screen.getByTestId('avatar-generate-loop')).toBeEnabled();
  }, 20_000);
});
