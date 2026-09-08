import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { buildInvokeRequest, requireLlmModel, startWikiChat, pollWikiChat } from './wikiChatApi';
import type { WikiChatTarget } from './wikiChatApi';

/**
 * MSW, NOT `vi.mock`. R-M1 allows a test to substitute the network boundary and
 * the socket double and nothing else, and it is the better test here anyway:
 * mocking the generated client would prove this module calls a function, while
 * intercepting the request proves it builds the right URL, sends the right body
 * and reads the right field out of what comes back.
 */

const BASE = 'http://elitea.test/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

const TARGET: WikiChatTarget = {
  projectId: 7,
  toolkitId: 42,
  toolkitName: 'wiki',
  toolkitType: 'deepwiki',
  settings: { toolkit_configuration_llm_model: 'gpt-5', toolkit_configuration_max_tokens: 8192 },
};

const INPUT = {
  toolName: 'ask',
  question: 'Where is the router?',
  history: [{ role: 'user' as const, content: 'earlier' }],
  capability: 'ask' as const,
  streamId: 'stream-1',
  messageId: 'message-1',
};

describe('the module imports the FUNCTION, not the query hook', () => {
  // The plan's named guard, and it is a SOURCE assertion because there is no
  // runtime signal: `useInvokeDeepWikiTool` would work — it would just fire the
  // POST on mount, on refocus and on reconnect, asking and billing the same
  // question several times while the screen looked correct. orval makes every
  // operation a query (`override.query.useQuery: true`), so the hook exists for
  // a mutation that must never be one.
  const source = readFileSync(
    resolve(dirname(fileURLToPath(import.meta.url)), 'wikiChatApi.ts'),
    'utf8',
  );

  // COMMENTS ARE STRIPPED FIRST. The doc comment above the module explains the
  // trap by NAME, and a raw substring search would fail on the explanation
  // rather than on an import — a gate that forbids writing down why it exists.
  const code = source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '');

  it('does not import any generated DeepWiki hook', () => {
    expect(code).toContain('invokeDeepWikiTool');
    expect(code).not.toMatch(/\buseInvokeDeepWikiTool\b/);
    expect(code).not.toMatch(/\buseGetDeepWikiInvocation\b/);
    // And the stripper itself must not be vacuous: the explanation IS in the
    // file, so a regex that ate everything would pass this suite silently.
    expect(source).toMatch(/\buseInvokeDeepWikiTool\b/);
    expect(code).toContain('export async function startWikiChat');
  });
});

describe('requireLlmModel', () => {
  it("reads the platform's own key, `llm_model`, before the legacy prefixed one", () => {
    expect(requireLlmModel({ llm_model: 'gpt-4o-mini' })).toBe('gpt-4o-mini');
    expect(requireLlmModel({ llm_model: 'gpt-4o-mini', toolkit_configuration_llm_model: 'old' })).toBe('gpt-4o-mini');
    expect(requireLlmModel({ toolkit_configuration_llm_model: 'old' })).toBe('old');
  });

  it('refuses a toolkit with no model, in the operator’s own terms', () => {
    // Sending the invocation anyway is accepted by the facade and fails inside
    // the provider, where the user is told nothing they can act on.
    expect(() => requireLlmModel({})).toThrow(/missing llm_model/);
    expect(() => requireLlmModel({ toolkit_configuration_llm_model: '' })).toThrow(/missing llm_model/);
  });
});

describe('buildInvokeRequest', () => {
  it('carries the question, the history and the model', () => {
    const request = buildInvokeRequest(TARGET, INPUT);
    expect(request.parameters).toMatchObject({
      question: 'Where is the router?',
      chat_history: [{ role: 'user', content: 'earlier' }],
      llm_model: 'gpt-5',
      llm_settings: { max_tokens: 8192, model_name: 'gpt-5' },
    });
    expect(request.configuration.parameters).toMatchObject({
      toolkit_configuration_llm_model: 'gpt-5',
    });
  });

  it('defaults max_tokens rather than sending undefined', () => {
    const request = buildInvokeRequest(
      { ...TARGET, settings: { toolkit_configuration_llm_model: 'gpt-5' } },
      INPUT,
    );
    expect(request.parameters['llm_settings']).toEqual({ max_tokens: 4096, model_name: 'gpt-5' });
  });

  it('adds the research parameters only for a research turn', () => {
    expect(buildInvokeRequest(TARGET, INPUT).parameters).not.toHaveProperty('enable_subagents');
    expect(
      buildInvokeRequest(TARGET, { ...INPUT, capability: 'research', toolName: 'deep_research' })
        .parameters,
    ).toMatchObject({ research_type: 'general', enable_subagents: true });
  });

  it('OMITS an empty override rather than sending it', () => {
    const withEmpty = buildInvokeRequest(
      { ...TARGET, repoIdentifierOverride: '', analysisKeyOverride: undefined },
      INPUT,
    );
    expect(withEmpty.parameters).not.toHaveProperty('repo_identifier_override');
    expect(withEmpty.parameters).not.toHaveProperty('analysis_key_override');

    const withValue = buildInvokeRequest({ ...TARGET, repoIdentifierOverride: 'acme/app' }, INPUT);
    expect(withValue.parameters['repo_identifier_override']).toBe('acme/app');
  });
});

describe('startWikiChat', () => {
  it('posts to the tool path and reads the invocation id out of the BODY', async () => {
    // Reading `invocation_id` off the transport ENVELOPE yields undefined on a
    // 200, and the drawer then polls `undefined` for ever with the spinner
    // still turning — issue #132's shape.
    let seen: { url: string; body: unknown } | null = null;
    server.use(
      http.post(`${BASE}/deepwiki/tools/:projectId/:toolkit/:tool/invoke`, async ({ request, params }) => {
        seen = { url: `${String(params['projectId'])}/${String(params['toolkit'])}/${String(params['tool'])}`, body: await request.json() };
        return HttpResponse.json({ invocation_id: 'inv-1' });
      }),
    );

    await expect(startWikiChat(TARGET, INPUT)).resolves.toBe('inv-1');
    expect(seen).not.toBeNull();
    expect(seen!.url).toBe('7/wiki/ask');
    expect(seen!.body).toMatchObject({ parameters: { question: 'Where is the router?' } });
  });

  it('fails loudly when the provider returns no invocation to follow', async () => {
    server.use(
      http.post(`${BASE}/deepwiki/tools/:projectId/:toolkit/:tool/invoke`, () =>
        HttpResponse.json({}),
      ),
    );
    await expect(startWikiChat(TARGET, INPUT)).rejects.toThrow(/no invocation to follow/);
  });
});

describe('pollWikiChat', () => {
  it('reads the poll off the invocation path', async () => {
    let seenPath: string | null = null;
    server.use(
      http.get(`${BASE}/deepwiki/invocations/:projectId/:toolkit/:tool/:invocation`, ({ params }) => {
        seenPath = ['projectId', 'toolkit', 'tool', 'invocation']
          .map((name) => String(params[name]))
          .join('/');
        return HttpResponse.json({
          status: 'InProgress',
          custom_events: [{ data: { message: 'a' } }],
        });
      }),
    );

    await expect(pollWikiChat(TARGET, 'ask', 'inv-1')).resolves.toMatchObject({
      status: 'InProgress',
      custom_events: [{ data: { message: 'a' } }],
    });
    expect(seenPath).toBe('7/wiki/ask/inv-1');
  });
});

describe('attached wiki pages', () => {
  it('sends the ids and the version pin together', () => {
    const request = buildInvokeRequest(
      { ...TARGET, wikiVersionId: 'fixture-1', contextPaths: ['wiki_pages/components/storage.md'] },
      INPUT,
    );
    expect(request.parameters).toMatchObject({
      context_paths: ['wiki_pages/components/storage.md'],
      context_wiki_version_id: 'fixture-1',
    });
  });

  it('sends page IDS, never page text', () => {
    // The whole reason the control is a picker over the manifest rather than a
    // free-text box or an upload: what leaves the browser can only NAME
    // something the wiki already published, so it cannot choose what the
    // server reads.
    const request = buildInvokeRequest(
      { ...TARGET, wikiVersionId: 'fixture-1', contextPaths: ['wiki_pages/a.md'] },
      INPUT,
    );
    const body = JSON.stringify(request.parameters);
    expect(body).toContain('wiki_pages/a.md');
    expect(body).not.toContain('http');
  });

  it('omits the selection when there is no version to pin it to', () => {
    // The provider REFUSES context_paths without a version, so sending the
    // selection alone would turn every attached question into an error the
    // reader cannot act on. A manifest with no wiki_version_id is that case:
    // better to answer unattached than to fail.
    const request = buildInvokeRequest({ ...TARGET, contextPaths: ['wiki_pages/a.md'] }, INPUT);
    expect(request.parameters).not.toHaveProperty('context_paths');
    expect(request.parameters).not.toHaveProperty('context_wiki_version_id');
  });

  it('omits both keys when nothing is attached', () => {
    const request = buildInvokeRequest({ ...TARGET, wikiVersionId: 'fixture-1' }, INPUT);
    expect(request.parameters).not.toHaveProperty('context_paths');
    expect(request.parameters).not.toHaveProperty('context_wiki_version_id');
  });
});
