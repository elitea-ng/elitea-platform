// The blueprint gallery's files (issue 3137): scripts/vite-blueprints.mjs with a stubbed builder.
import { describe, expect, it, vi } from 'vitest';

import { blueprintContentType, blueprintRequestPath, blueprintsPlugin } from './vite-blueprints.mjs';

const FILES = new Map([
  ['index.json', Buffer.from('{"schema":"elitea.blueprints/v1"}')],
  ['agents/scout.agent.md', Buffer.from('---\nname: Scout\n---\n')],
  ['teams/desk.team.zip', Buffer.from('PK')],
]);

function stubBuild() {
  return vi.fn(async () => ({ files: FILES, index: {} }));
}

function devMiddleware(plugin) {
  let middleware;
  plugin.configureServer({ middlewares: { use: (fn) => (middleware = fn) } });
  return (url) =>
    new Promise((resolve, reject) => {
      const headers = {};
      const res = {
        setHeader: (name, value) => (headers[name] = value),
        end: (body) => resolve({ body, headers }),
      };
      middleware({ url }, res, (error) => (error === undefined ? resolve(undefined) : reject(error)));
    });
}

describe('blueprintsPlugin', () => {
  it('emits every built file under blueprints/ in the bundle, building once', async () => {
    const build = stubBuild();
    const plugin = blueprintsPlugin({ root: '/repo', build });
    const emitted = [];
    const context = { emitFile: (file) => emitted.push(file) };
    await plugin.generateBundle.call(context);
    await plugin.generateBundle.call(context);
    expect(build).toHaveBeenCalledTimes(1);
    expect(build).toHaveBeenCalledWith('/repo');
    expect(emitted.slice(0, 3).map((file) => [file.type, file.fileName])).toEqual([
      ['asset', 'blueprints/index.json'],
      ['asset', 'blueprints/agents/scout.agent.md'],
      ['asset', 'blueprints/teams/desk.team.zip'],
    ]);
    expect(emitted[0].source).toBe(FILES.get('index.json'));
  });

  it('serves a built file in dev with its content type, under any base', async () => {
    const request = devMiddleware(blueprintsPlugin({ build: stubBuild() }));
    const index = await request('/app/blueprints/index.json?t=1');
    expect(index.headers['Content-Type']).toBe('application/json');
    expect(index.body).toBe(FILES.get('index.json'));
    const team = await request('/blueprints/teams/desk.team.zip');
    expect(team.headers['Content-Type']).toBe('application/zip');
  });

  it('passes on what it does not hold: another path, an unknown file, a parent segment', async () => {
    const build = stubBuild();
    const request = devMiddleware(blueprintsPlugin({ build }));
    expect(await request('/app/index.html')).toBeUndefined();
    expect(build).not.toHaveBeenCalled();
    expect(await request('/blueprints/agents/missing.agent.md')).toBeUndefined();
    expect(await request('/blueprints/../package.json')).toBeUndefined();
  });

  it('hands a failed build to the dev server instead of answering', async () => {
    const request = devMiddleware(blueprintsPlugin({ build: async () => Promise.reject(new Error('bad blueprint')) }));
    await expect(request('/blueprints/index.json')).rejects.toThrow('bad blueprint');
  });
});

describe('blueprint helpers', () => {
  it('names the content type by extension', () => {
    expect(blueprintContentType('agents/a.agent.md')).toMatch(/^text\/markdown/);
    expect(blueprintContentType('avatars/a/still.webp')).toBe('image/webp');
    expect(blueprintContentType('release-notes')).toBe('application/octet-stream');
  });

  it('reads the built path from a request URL', () => {
    expect(blueprintRequestPath('/app/blueprints/avatars/x%20y.png')).toBe('avatars/x y.png');
    expect(blueprintRequestPath('/app/assets/blueprints.js')).toBeUndefined();
    expect(blueprintRequestPath(undefined)).toBeUndefined();
  });
});
