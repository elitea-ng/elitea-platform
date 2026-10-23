import { describe, expect, it } from 'vitest';

import { resolveShotRoute } from './docs-shot-route.mjs';

describe('resolveShotRoute', () => {
  it('resolves placeholders by name and through a shot override', () => {
    expect(resolveShotRoute('/app/agents/:agentId', undefined, { agentId: 7 })).toEqual({
      route: '/app/agents/7',
      error: undefined,
    });
    expect(
      resolveShotRoute('/app/toolkits/:toolkitId', { toolkitId: 'openapiToolkitId' }, {
        toolkitId: 'github',
        openapiToolkitId: 'openapi',
      }),
    ).toEqual({ route: '/app/toolkits/openapi', error: undefined });
  });

  it('leaves a placeholder-free route alone with an empty seed map', () => {
    expect(resolveShotRoute('/app/agents-hub', undefined, {})).toEqual({
      route: '/app/agents-hub',
      error: undefined,
    });
  });

  // #1342: the ci-web-e2e dispatch seeds no wiki toolkit; the DeepWiki shot
  // must come back as a per-shot skip, not a throw that aborts the run.
  it('returns a skip, not a throw, when a seed key is absent', () => {
    let result;
    expect(() => {
      result = resolveShotRoute('/app/deepwiki/:toolkitId', { toolkitId: 'wikiToolkitId' }, { toolkitId: 'github' });
    }).not.toThrow();
    expect(result.route).toBe('/app/deepwiki/:toolkitId');
    expect(result.error).toMatch(/^skipped: route placeholder ":toolkitId" \(seed map key "wikiToolkitId"\)/);
    expect(result.error).toContain('got undefined');
  });

  it('refuses a value that is neither a string nor a number', () => {
    const result = resolveShotRoute('/app/pipelines/:pipelineId', undefined, { pipelineId: null });
    expect(result.error).toContain('got null');
  });
});
