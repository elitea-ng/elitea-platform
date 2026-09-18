/**
 * Contract tests for the agent icon endpoints (`elitea_issues: #6627`).
 *
 * Two routes, asserted separately because the Go side keeps them separate:
 * upload (POST, storage only) never touches a version; bind (PUT) is the
 * only write that reaches `application_versions.meta.icon_meta`. A caller
 * that conflated them (or fired only one) would leave a freshly uploaded
 * file unreachable from any version — the exact class of bug `#6627` was
 * filed over, one layer down from the UI.
 */
import { HttpResponse, http } from 'msw';
import { beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../test/setup';

import { bindApplicationIcon, uploadApplicationIconFile } from './applicationIconApi';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

describe('applicationIconApi', () => {
  it('uploadApplicationIconFile posts multipart and returns the icon_meta the server minted', async () => {
    let seenType = '';
    let seenBody = '';
    server.use(
      http.post(`${BASE}/elitea_core/upload_icon/prompt_lib/7/0`, async ({ request }) => {
        seenType = request.headers.get('content-type') ?? '';
        seenBody = await request.text();
        return HttpResponse.json({
          ok: true,
          url: '/icons/7/abc.png',
          icon_meta: { url: '/icons/7/abc.png', width: 64, height: 64 },
        });
      }),
    );

    const meta = await uploadApplicationIconFile('7', { file: new File(['x'], 'a.png', { type: 'image/png' }) });

    expect(seenType).toMatch(/^multipart\/form-data/);
    expect(seenBody).toContain('name="file"');
    expect(meta).toEqual({ url: '/icons/7/abc.png', width: 64, height: 64 });
  });

  it('uploadApplicationIconFile returns null on the "no file" fast path', async () => {
    server.use(
      http.post(`${BASE}/elitea_core/upload_icon/prompt_lib/7/0`, () => HttpResponse.json({ ok: true, url: '' })),
    );

    const meta = await uploadApplicationIconFile('7', { file: new File([], 'empty.png') });
    expect(meta).toBeNull();
  });

  it('bindApplicationIcon PUTs the chosen icon meta at the version path', async () => {
    let body: unknown;
    server.use(
      http.put(`${BASE}/elitea_core/upload_icon/prompt_lib/7/42`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ ok: true });
      }),
    );

    await bindApplicationIcon('7', {
      applicationId: 9,
      versionId: '42',
      iconMeta: { name: 'image_0.png', url: '/app/default_entity_icons/image_0.png' },
    });
    expect(body).toEqual({ name: 'image_0.png', url: '/app/default_entity_icons/image_0.png' });
  });

  it('bindApplicationIcon sends an empty body to reset to the fallback glyph', async () => {
    let body: unknown;
    server.use(
      http.put(`${BASE}/elitea_core/upload_icon/prompt_lib/7/42`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ ok: true });
      }),
    );

    await bindApplicationIcon('7', { applicationId: 9, versionId: '42', iconMeta: null });
    expect(body).toEqual({});
  });
});
