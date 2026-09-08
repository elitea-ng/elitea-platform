/**
 * The live lanes' environment contract, checked where it can be read in one
 * second rather than discovered on a machine that holds a real credential.
 *
 * Three properties, and none of them is visible from inside a journey:
 *
 *  1. A provider is configured only when EVERY required variable is set.
 *     A rule that accepted "some of them" would list a spec that then failed
 *     on a missing base URL, which reads as a broken provider.
 *  2. An empty string is ABSENT. `secrets.E2E_LIVE_GITHUB_TOKEN` on a
 *     repository with no such secret expands to the empty string in a
 *     workflow's `env:` block, so a lane that treated `''` as configured
 *     would arm itself on every continuous-integration run and fail against
 *     GitHub with an empty token.
 *  3. `probeEvidence()` is NOT quotable from `probePrompt`. That string is
 *     what replaces the legacy Test-Settings panel's raw-result read; if the
 *     prompt contained it, a model that never called the tool could satisfy
 *     the assertion by repeating the question — the journey would go green on
 *     a toolkit that was never materialised.
 *
 * Runs in the `scripts` vitest project, like every other gate self-test here.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  configuredLiveToolkits,
  isLiveToolkitConfigured,
  liveEnvNames,
  liveImageModel,
  LIVE_TOOLKIT_IDS,
  LIVE_TOOLKIT_PROVIDERS,
} from '../e2e/live/liveEnv.ts';

/** Every `E2E_LIVE_*` value this process started with, restored after each test. */
let saved = {};

beforeEach(() => {
  saved = {};
  for (const name of Object.keys(process.env)) {
    if (name.startsWith('E2E_LIVE_')) {
      saved[name] = process.env[name];
      delete process.env[name];
    }
  }
});

afterEach(() => {
  for (const name of Object.keys(process.env)) {
    if (name.startsWith('E2E_LIVE_')) delete process.env[name];
  }
  for (const [name, value] of Object.entries(saved)) process.env[name] = value;
});

describe('a provider is configured only when all of its variables are', () => {
  it('reports nothing configured on a bare environment', () => {
    expect(configuredLiveToolkits()).toEqual([]);
    expect(liveImageModel()).toBe('');
  });

  it('rejects a provider that is only half set', () => {
    process.env['E2E_LIVE_JIRA_BASE_URL'] = 'https://jira.example';
    process.env['E2E_LIVE_JIRA_USERNAME'] = 'someone';
    expect(isLiveToolkitConfigured('jira')).toBe(false);

    process.env['E2E_LIVE_JIRA_API_KEY'] = 'k';
    process.env['E2E_LIVE_JIRA_PROJECT_KEY'] = 'ABC';
    expect(isLiveToolkitConfigured('jira')).toBe(true);
    expect(configuredLiveToolkits()).toEqual(['jira']);
  });

  it('treats an empty value as absent — a repository with no such secret expands to ""', () => {
    for (const name of LIVE_TOOLKIT_PROVIDERS.github.requiredEnv) process.env[name] = '';
    expect(isLiveToolkitConfigured('github')).toBe(false);
    process.env['E2E_LIVE_IMAGE_MODEL'] = '';
    expect(liveImageModel()).toBe('');
  });

  it('trims, so a trailing newline out of a secret store is not a credential', () => {
    process.env['E2E_LIVE_IMAGE_MODEL'] = '  openai/dall-e-3\n';
    expect(liveImageModel()).toBe('openai/dall-e-3');
  });
});

describe('every provider carries a usable descriptor', () => {
  it('names a required variable, a probe tool and answer keywords', () => {
    for (const id of LIVE_TOOLKIT_IDS) {
      const provider = LIVE_TOOLKIT_PROVIDERS[id];
      expect(provider.id, `${id} descriptor is keyed by its own id`).toBe(id);
      expect(provider.requiredEnv.length, `${id} declares no required variable`).toBeGreaterThan(0);
      for (const name of [...provider.requiredEnv, ...provider.optionalEnv]) {
        expect(name, `${id} reads ${name}, which is not E2E_LIVE_-prefixed`).toMatch(/^E2E_LIVE_/);
      }
      expect(provider.probeTool, `${id} names no probe tool`).not.toBe('');
      expect(provider.probePrompt, `${id}'s probe prompt must name its tool`).toContain(
        provider.probeTool,
      );
      expect(provider.answerKeywords.length, `${id} declares no answer keyword`).toBeGreaterThan(0);
    }
  });

  it('never lets the evidence string be quoted out of the prompt', () => {
    // Set every variable to a value that cannot occur in a prompt by accident.
    for (const name of liveEnvNames()) process.env[name] = `zz-${name.toLowerCase()}-zz`;
    for (const id of LIVE_TOOLKIT_IDS) {
      const provider = LIVE_TOOLKIT_PROVIDERS[id];
      const evidence = provider.probeEvidence();
      expect(evidence, `${id} has no evidence string to assert on`).not.toBe('');
      expect(
        provider.probePrompt.toLowerCase().includes(evidence.toLowerCase()),
        `${id}'s probe prompt contains its own evidence string — a model that never called ` +
          'the tool could satisfy the journey by repeating the question',
      ).toBe(false);
    }
  });

  it('falls back to a working default for every optional variable', () => {
    process.env['E2E_LIVE_GITHUB_TOKEN'] = 't';
    process.env['E2E_LIVE_GITHUB_REPOSITORY'] = 'owner/name';
    const data = LIVE_TOOLKIT_PROVIDERS.github.credentialData();
    expect(data['base_url']).toBe('https://api.github.com');
    expect(LIVE_TOOLKIT_PROVIDERS.github.probeEvidence()).toBe('main');
  });

  it('replaces only the SECRET in the broken payload, so the refusal is an auth refusal', () => {
    process.env['E2E_LIVE_JIRA_BASE_URL'] = 'https://jira.example';
    process.env['E2E_LIVE_JIRA_USERNAME'] = 'someone';
    process.env['E2E_LIVE_JIRA_API_KEY'] = 'the-real-key';
    process.env['E2E_LIVE_JIRA_PROJECT_KEY'] = 'ABC';
    const good = LIVE_TOOLKIT_PROVIDERS.jira.credentialData();
    const bad = LIVE_TOOLKIT_PROVIDERS.jira.brokenCredentialData();
    expect(bad['base_url']).toBe(good['base_url']);
    expect(bad['username']).toBe(good['username']);
    expect(bad['api_key']).not.toBe(good['api_key']);
    expect(bad['api_key']).not.toBe('');
  });

  it('keeps the credential reference the toolkit settings are read through', () => {
    for (const id of LIVE_TOOLKIT_IDS) {
      const settings = LIVE_TOOLKIT_PROVIDERS[id].toolkitSettings('autotest_title');
      expect(
        settings[`${id}_configuration`],
        `${id} toolkit settings must carry a ${id}_configuration reference`,
      ).toEqual({ elitea_title: 'autotest_title', private: true });
    }
  });
});
