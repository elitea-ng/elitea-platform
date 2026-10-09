import { describe, expect, it, vi } from 'vitest';

import { desktopRuntimeConfig, readPublicProjectId } from './deploymentConfig';

describe('desktopRuntimeConfig', () => {
  it('makes the API base absolute against the deployment origin and drops a trailing slash', () => {
    expect(desktopRuntimeConfig('https://elitea.example.com/', '1')).toEqual({
      vite_server_url: 'https://elitea.example.com/api/v2',
      vite_base_uri: '/',
      vite_public_project_id: '1',
    });
  });
});

describe('readPublicProjectId', () => {
  const configJs = `window.elitea_ui_config = {\n  vite_server_url: "/api/v2",\n  vite_public_project_id: "42"\n};`;

  it('reads the id out of the deployment config.js without evaluating it', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response(configJs));
    expect(await readPublicProjectId(fetchMock, 'https://elitea.example.com')).toBe('42');
    expect(fetchMock.mock.calls[0]?.[0]).toBe('https://elitea.example.com/app/config.js');
  });

  it.each([
    ['a 404', () => new Response('', { status: 404 })],
    ['no id in the file', () => new Response('window.elitea_ui_config = {}')],
  ])('yields an empty id for %s', async (_label, make) => {
    expect(await readPublicProjectId(vi.fn<typeof fetch>().mockResolvedValue(make()), 'https://h')).toBe('');
  });

  it('yields an empty id when the request fails', async () => {
    expect(await readPublicProjectId(vi.fn<typeof fetch>().mockRejectedValue(new TypeError('x')), 'https://h')).toBe('');
  });
});
