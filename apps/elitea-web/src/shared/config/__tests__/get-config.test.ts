import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getConfig, type Config, type ConfigKey, type ConfigResult } from '../index';
import { resetConfigForTests } from '../get-config';

/**
 * C6/C7/C7b contract tests (spec §7.1; §9.3 unit F3).
 *
 * Expectations are HARD-CODED literals on purpose: deriving them from the
 * production constants would make the mutation proofs (§6.7 discipline)
 * vacuous — dropping a key from the required set must FAIL a test here.
 *
 * How each C6 source is driven (verified against vitest@4.1.10 internals,
 * node_modules/vitest/dist/chunks/init.k9zZ9sLh.js:174 — import.meta.env is a
 * Proxy whose TARGET is the real process.env object captured at worker init:
 * `in` checks hit that target, while `get`/`set` delegate to the CURRENT
 * globalThis.process.env):
 *  - window.elitea_ui_config  → assigned on globalThis (jsdom: window === globalThis)
 *  - import.meta.env          → vi.stubEnv (writes the real process.env, which
 *                               is both the proxy target and its live store)
 *  - globalThis.__ENV__       → assigned on globalThis
 *  - process.env              → vi.stubGlobal('process', { env: {...} })
 *
 * SOURCE 2 AND SOURCE 4 ALIAS IN THIS ENVIRONMENT. In a real browser bundle
 * they are wholly distinct (Vite inlines import.meta.env; `process` does not
 * exist). Under vitest, import.meta.env's VALUE always comes from whatever
 * globalThis.process.env currently is — the very object source 4 reads — so
 * NO value-based assertion can distinguish them. Only two things differ:
 * presence (`in` consults the real captured env, not a stubbed fake) and the
 * source NAME this module records. The 'sources 2 vs 4' block below therefore
 * discriminates on ATTRIBUTION: it arranges one invalid value that both
 * sources would read identically, and asserts which source name is reported.
 *
 * MUTATION SENSITIVITY of the resolution-order matrix, measured (not assumed)
 * by swapping entries in sources.ts and running this suite:
 *  - swap sources 2↔3 (import.meta.env ↔ __ENV__) → the five
 *    "import.meta.env wins over __ENV__" rows fail.
 *  - swap sources 2↔4 (import.meta.env ↔ process.env) → the five
 *    "__ENV__ wins over process.env" rows fail, plus the attribution test
 *    below. It is NOT the "import.meta.env wins over __ENV__" rows that
 *    fail, because vi.stubEnv writes the same store source 4 reads — the
 *    aliasing above.
 *  - swap sources 1↔2 → the five "window.elitea_ui_config wins…" rows fail.
 * The order is thus pinned pairwise across all four sources.
 *
 * `allow_project_own_llms` (§7.1 C7 gap found by unit R1 while porting
 * IntegrationGuard.jsx; folded into ConfigSchema here) is NOT part of the
 * ALL_KEYS-driven matrix above: unlike its five siblings, the old app's own
 * call site passed it to getEnvVar already lower-case
 * (`getEnvVar('allow_project_own_llms', true)`, constants.js:15), so sources
 * 2-4 look it up by its VERBATIM lower-case name, not `.toUpperCase()`. It
 * gets its own parallel matrix + default/no-coercion tests below instead of
 * being folded into the generic one, which would silently paper over that
 * casing difference.
 */

const REQUIRED_KEYS = ['vite_server_url', 'vite_base_uri', 'vite_public_project_id'] as const;
const ALL_KEYS = [
  'vite_server_url',
  'vite_base_uri',
  'vite_socket_server',
  'vite_socket_path',
  'vite_public_project_id',
] as const;

const VALID_TRIO = {
  vite_server_url: 'https://elitea.example',
  vite_base_uri: '/app/',
  vite_public_project_id: '11',
} as const;

/** VALID_TRIO plus the default-true 6th key every 'ok' config now carries. */
const VALID_TRIO_KEYS_SORTED = [
  'allow_project_own_llms',
  'vite_base_uri',
  'vite_public_project_id',
  'vite_server_url',
] as const;

const g = globalThis as unknown as Record<string, unknown>;
const realProcessEnv = (g['process'] as { env: Record<string, string | undefined> }).env;

function setWindowConfig(config: Record<string, unknown>): void {
  g['elitea_ui_config'] = config;
}

function setEnvGlobal(env: Record<string, unknown>): void {
  g['__ENV__'] = env;
}

function trioWithout(key: string): Record<string, unknown> {
  const config: Record<string, unknown> = { ...VALID_TRIO };
  // eslint-style guard not needed: key comes from the hard-coded lists above
  delete config[key];
  return config;
}

function expectOk(result: ConfigResult): Config {
  if (result.status !== 'ok') {
    throw new Error(`expected ok result, got missing: [${result.missing.join(', ')}]`);
  }
  return result.config;
}

function getConfigWithStubbedProcess(fakeProcess: unknown): ConfigResult {
  vi.stubGlobal('process', fakeProcess);
  try {
    return getConfig();
  } finally {
    vi.unstubAllGlobals();
  }
}

beforeEach(() => {
  resetConfigForTests();
  // Shell-provided VITE_* vars would leak into sources 2 and 4; clear them.
  for (const key of ALL_KEYS) {
    delete realProcessEnv[key.toUpperCase()];
  }
  delete realProcessEnv['VITE_DEV_TOKEN'];
  // allow_project_own_llms uses its VERBATIM (lower-case) name, not .toUpperCase().
  delete realProcessEnv['allow_project_own_llms'];
});

afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
  delete g['elitea_ui_config'];
  delete g['__ENV__'];
  resetConfigForTests();
});

describe.each([...ALL_KEYS])('C6 resolution order for %s', (key: ConfigKey) => {
  const envName = key.toUpperCase();

  it('window.elitea_ui_config wins over import.meta.env, __ENV__ and process.env', () => {
    setWindowConfig({ ...trioWithout(key), [key]: 'from-window' });
    vi.stubEnv(envName, 'from-import-meta-env'); // also lands in real process.env
    setEnvGlobal({ [envName]: 'from-env-global' });

    expect(expectOk(getConfig())[key]).toBe('from-window');
  });

  it('import.meta.env wins over __ENV__ (and everything below it)', () => {
    setWindowConfig(trioWithout(key));
    vi.stubEnv(envName, 'from-import-meta-env');
    setEnvGlobal({ [envName]: 'from-env-global' });

    expect(expectOk(getConfig())[key]).toBe('from-import-meta-env');
  });

  it('globalThis.__ENV__ wins over process.env', () => {
    setWindowConfig(trioWithout(key));
    setEnvGlobal({ [envName]: 'from-env-global' });
    const result = getConfigWithStubbedProcess({ env: { [envName]: 'from-process-env' } });

    expect(expectOk(result)[key]).toBe('from-env-global');
  });

  it('process.env is the last-resort source', () => {
    setWindowConfig(trioWithout(key));
    const result = getConfigWithStubbedProcess({ env: { [envName]: 'from-process-env' } });

    expect(expectOk(result)[key]).toBe('from-process-env');
  });
});

describe('C6 resolution order for allow_project_own_llms (verbatim lower-case name)', () => {
  const KEY = 'allow_project_own_llms';

  it('window.elitea_ui_config wins over import.meta.env, __ENV__ and process.env', () => {
    setWindowConfig({ ...VALID_TRIO, [KEY]: 'from-window' });
    vi.stubEnv(KEY, 'from-import-meta-env');
    setEnvGlobal({ [KEY]: 'from-env-global' });

    expect(expectOk(getConfig())[KEY]).toBe('from-window');
  });

  it('import.meta.env wins over __ENV__ (and everything below it)', () => {
    setWindowConfig({ ...VALID_TRIO });
    vi.stubEnv(KEY, 'from-import-meta-env');
    setEnvGlobal({ [KEY]: 'from-env-global' });

    expect(expectOk(getConfig())[KEY]).toBe('from-import-meta-env');
  });

  it('globalThis.__ENV__ wins over process.env', () => {
    setWindowConfig({ ...VALID_TRIO });
    setEnvGlobal({ [KEY]: 'from-env-global' });
    const result = getConfigWithStubbedProcess({ env: { [KEY]: 'from-process-env' } });

    expect(expectOk(result)[KEY]).toBe('from-env-global');
  });

  it('process.env is the last-resort source', () => {
    setWindowConfig({ ...VALID_TRIO });
    const result = getConfigWithStubbedProcess({ env: { [KEY]: 'from-process-env' } });

    expect(expectOk(result)[KEY]).toBe('from-process-env');
  });

  // A dedicated proof that this key's lookup name is NOT `.toUpperCase()`-d
  // like its five siblings: setting only the UPPER-CASE variant must be
  // invisible to every source, leaving the true default (true) in place.
  it('the UPPER_CASE variant of the name is never consulted by any source', () => {
    setWindowConfig({ ...VALID_TRIO, ALLOW_PROJECT_OWN_LLMS: 'wrong-cased-window-hit' });
    vi.stubEnv('ALLOW_PROJECT_OWN_LLMS', 'wrong-cased-env');
    setEnvGlobal({ ALLOW_PROJECT_OWN_LLMS: 'wrong-cased-global' });

    expect(expectOk(getConfig())[KEY]).toBe(true);
  });
});

describe('allow_project_own_llms — unparsed passthrough + getEnvVar(key, true) default (§7.1 C7 gap, unit R1)', () => {
  const KEY = 'allow_project_own_llms';

  it('defaults to the literal `true` old getEnvVar passed as its fallback when no source defines it', () => {
    setWindowConfig({ ...VALID_TRIO }); // key present nowhere

    expect(expectOk(getConfig())[KEY]).toBe(true);
  });

  it('passes through a raw string "false" WITHOUT coercing it to boolean (IntegrationGuard.jsx:13 strict === false parity)', () => {
    setWindowConfig({ ...VALID_TRIO, [KEY]: 'false' });

    const value = expectOk(getConfig())[KEY];
    expect(value).toBe('false');
    expect(value).not.toBe(false);
    // eslint-disable-next-line typescript/no-unnecessary-boolean-literal-compare -- deliberately asserting the OLD app's surprising non-coercion (N4), not writing idiomatic boolean logic.
    expect(value === false).toBe(false);
  });

  it('passes through a raw boolean false unchanged', () => {
    setWindowConfig({ ...VALID_TRIO, [KEY]: false });

    expect(expectOk(getConfig())[KEY]).toBe(false);
  });

  it('passes through a raw number unchanged (never validated as a boolean)', () => {
    setWindowConfig({ ...VALID_TRIO, [KEY]: 0 });

    expect(expectOk(getConfig())[KEY]).toBe(0);
  });

  it('an own key defined as undefined resolves to the true default (documented zod-default simplification, schema.ts)', () => {
    setWindowConfig({ ...VALID_TRIO, [KEY]: undefined });
    setEnvGlobal({ [KEY]: 'from-env-global' }); // shadowed; must not be reached

    expect(expectOk(getConfig())[KEY]).toBe(true);
  });

  it('never appears in a zod-invalid `reasons` entry — z.unknown() cannot fail validation', () => {
    setWindowConfig({ ...VALID_TRIO, [KEY]: { nested: 'object' } });

    const config = expectOk(getConfig());
    expect(config[KEY]).toEqual({ nested: 'object' });
  });
});

describe('sources 2 vs 4 — distinguished directly, by attribution', () => {
  // Both tests feed the SAME invalid value (a number, which only a fake env
  // object can hold) through the SAME underlying store, so the only thing
  // that can differ between them is which source claimed the key.
  const FAKE_PROCESS = { env: { VITE_SERVER_URL: 12345, VITE_BASE_URI: '/app/' } };

  it('import.meta.env wins: the key it defines is attributed to source 2', () => {
    setWindowConfig({ vite_public_project_id: '1' });
    // Present in the REAL captured env ⇒ the proxy's `in` trap reports it ⇒
    // source 2 legitimately defines the key. Its VALUE, per the aliasing
    // note above, is read back from the stubbed process.env below.
    vi.stubEnv('VITE_SERVER_URL', 'present-in-the-real-env');

    const result = getConfigWithStubbedProcess(FAKE_PROCESS);

    expect(result.status).toBe('missing');
    if (result.status !== 'missing') {
      throw new Error('unreachable');
    }
    // Swapping sources 2 and 4 in sources.ts flips this to 'process.env'.
    expect(result.reasons['vite_server_url']).toMatch(
      /^invalid value from import\.meta\.env: /,
    );
    expect(result.reasons['vite_server_url']).not.toMatch(/from process\.env/);
  });

  it('process.env is attributed only when import.meta.env does not define the key', () => {
    setWindowConfig({ vite_public_project_id: '1' });
    // No vi.stubEnv here: VITE_SERVER_URL is absent from the real captured
    // env (beforeEach deleted it), so the proxy's `in` is false and source 2
    // legitimately skips — leaving the identical value to source 4.

    const result = getConfigWithStubbedProcess(FAKE_PROCESS);

    expect(result.status).toBe('missing');
    if (result.status !== 'missing') {
      throw new Error('unreachable');
    }
    expect(result.reasons['vite_server_url']).toMatch(/^invalid value from process\.env: /);
  });
});

describe('C7b — vite_dev_token and dev are removed from the contract (D10)', () => {
  it('ignores them in every source and never surfaces them on Config', () => {
    setWindowConfig({
      ...VALID_TRIO,
      vite_socket_server: 'wss://sock.example',
      vite_socket_path: '/socket.io',
      vite_dev_token: 'window-secret',
      dev: '1',
    });
    vi.stubEnv('VITE_DEV_TOKEN', 'env-secret');
    setEnvGlobal({ VITE_DEV_TOKEN: 'env-global-secret', DEV: '1' });

    const config = expectOk(getConfig());

    expect(Object.keys(config).sort()).toEqual(
      [
        'vite_server_url',
        'vite_base_uri',
        'vite_socket_server',
        'vite_socket_path',
        'vite_public_project_id',
        'allow_project_own_llms',
      ].sort(),
    );
    expect('vite_dev_token' in config).toBe(false);
    expect('dev' in config).toBe(false);
    // No index-signature leakage at runtime…
    expect((config as unknown as Record<string, unknown>)['vite_dev_token']).toBeUndefined();
    expect((config as unknown as Record<string, unknown>)['dev']).toBeUndefined();
    // …and none at the type level either (tsc fails if Config grows these).
    // @ts-expect-error -- C7b: Config must not expose vite_dev_token
    void config.vite_dev_token;
    // @ts-expect-error -- C7b: Config must not expose dev
    void config.dev;
  });
});

describe('missing required vars → discriminated result (§3.6: errors are values)', () => {
  it.each([...REQUIRED_KEYS])('reports %s when it is the only var missing', (key) => {
    setWindowConfig(trioWithout(key));

    const result = getConfig();
    expect(result.status).toBe('missing');
    if (result.status !== 'missing') {
      throw new Error('unreachable');
    }
    expect(result.missing).toEqual([key]);
    expect(result.reasons[key]).toBe('not defined in any config source');
    expect(Object.keys(result.reasons)).toEqual([key]);
  });

  it.each([
    [['vite_server_url', 'vite_base_uri']],
    [['vite_server_url', 'vite_public_project_id']],
    [['vite_base_uri', 'vite_public_project_id']],
  ])('reports the pair %j in C7 contract order', (pair) => {
    const config: Record<string, unknown> = { ...VALID_TRIO };
    for (const key of pair) {
      delete config[key];
    }
    setWindowConfig(config);

    const result = getConfig();
    expect(result.status).toBe('missing');
    if (result.status !== 'missing') {
      throw new Error('unreachable');
    }
    expect(result.missing).toEqual(pair);
  });

  it('reports all three in C7 contract order when no source defines anything', () => {
    const result = getConfig();
    expect(result.status).toBe('missing');
    if (result.status !== 'missing') {
      throw new Error('unreachable');
    }
    expect(result.missing).toEqual(['vite_server_url', 'vite_base_uri', 'vite_public_project_id']);
  });

  it('treats an empty runtime-config object the same as no config at all', () => {
    setWindowConfig({});

    const result = getConfig();
    expect(result.status).toBe('missing');
    if (result.status !== 'missing') {
      throw new Error('unreachable');
    }
    expect(result.missing).toEqual(['vite_server_url', 'vite_base_uri', 'vite_public_project_id']);
  });

  it('missing optional keys with no default do not affect the ok result', () => {
    setWindowConfig({ ...VALID_TRIO });

    const config = expectOk(getConfig());
    expect('vite_socket_server' in config).toBe(false);
    expect('vite_socket_path' in config).toBe(false);
    // allow_project_own_llms is also optional, but unlike the socket keys it
    // HAS a default (true) — so it IS present, unlike the socket keys.
    expect(Object.keys(config).sort()).toEqual([...VALID_TRIO_KEYS_SORTED]);
  });

  it('allow_project_own_llms being absent everywhere never appears in `missing`, even as the only unresolved key', () => {
    setWindowConfig({ ...VALID_TRIO }); // required trio present; 6th key absent from all 4 sources

    const result = getConfig();
    expect(result.status).toBe('ok');
  });
});

describe('zod-invalid values are treated as missing, with a reason', () => {
  it('flags a non-string required value with source attribution', () => {
    setWindowConfig({ ...VALID_TRIO, vite_server_url: 123 });

    const result = getConfig();
    expect(result.status).toBe('missing');
    if (result.status !== 'missing') {
      throw new Error('unreachable');
    }
    expect(result.missing).toEqual(['vite_server_url']);
    expect(result.reasons['vite_server_url']).toMatch(
      /^invalid value from window\.elitea_ui_config: /,
    );
    expect(result.reasons['vite_server_url']).toMatch(/expected string/i);
  });

  it('flags a null required value (old MISSING_ENVS parity: null counted as missing)', () => {
    setWindowConfig({ ...VALID_TRIO, vite_base_uri: null });

    const result = getConfig();
    expect(result.status).toBe('missing');
    if (result.status !== 'missing') {
      throw new Error('unreachable');
    }
    expect(result.missing).toEqual(['vite_base_uri']);
    expect(result.reasons['vite_base_uri']).toMatch(/^invalid value from/);
  });

  it('an invalid definition still wins the C6 race — later sources are not consulted', () => {
    setWindowConfig({ ...VALID_TRIO, vite_server_url: 123 });
    setEnvGlobal({ VITE_SERVER_URL: 'https://would-be-valid.example' });

    const result = getConfig();
    expect(result.status).toBe('missing');
    if (result.status !== 'missing') {
      throw new Error('unreachable');
    }
    expect(result.missing).toEqual(['vite_server_url']);
  });

  it('drops an invalid optional value without failing the config', () => {
    setWindowConfig({ ...VALID_TRIO, vite_socket_path: 42 });

    const config = expectOk(getConfig());
    expect('vite_socket_path' in config).toBe(false);
  });
});

describe('getEnvVar parity edge cases (env.js:4-33)', () => {
  it('an own key defined as undefined shadows later sources and yields no value', () => {
    setWindowConfig({ ...VALID_TRIO, vite_socket_server: undefined });
    setEnvGlobal({ VITE_SOCKET_SERVER: 'wss://later.example' });

    const config = expectOk(getConfig());
    expect('vite_socket_server' in config).toBe(false);
  });

  it('a required key defined as undefined shadows later sources and stays missing', () => {
    setWindowConfig({ ...VALID_TRIO, vite_server_url: undefined });
    setEnvGlobal({ VITE_SERVER_URL: 'https://later.example' });

    const result = getConfig();
    expect(result.status).toBe('missing');
    if (result.status !== 'missing') {
      throw new Error('unreachable');
    }
    expect(result.missing).toEqual(['vite_server_url']);
    expect(result.reasons['vite_server_url']).toBe('not defined in any config source');
  });

  it('inherited runtime-config keys do not count (hasOwnProperty parity)', () => {
    const proto = { vite_socket_server: 'wss://inherited.example' };
    const config = Object.assign(
      Object.create(proto) as Record<string, unknown>,
      VALID_TRIO,
    );
    setWindowConfig(config);

    const resolved = expectOk(getConfig());
    expect('vite_socket_server' in resolved).toBe(false);
  });

  it('falls back to window.elitea_ui_config when globalThis has no copy (env.js:9-10)', () => {
    vi.stubGlobal('window', {
      elitea_ui_config: { ...VALID_TRIO, vite_socket_path: '/from-window-object' },
    });
    try {
      const config = expectOk(getConfig());
      expect(config.vite_socket_path).toBe('/from-window-object');
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('reads own keys off a FUNCTION-valued elitea_ui_config, as the old code did', () => {
    // env.js used hasOwnProperty/`in`, which work on functions; treating a
    // function as "absent" would be a silent behaviour divergence.
    const configFn = Object.assign(() => undefined, VALID_TRIO, {
      vite_socket_path: '/from-a-function',
    });
    g['elitea_ui_config'] = configFn;

    const config = expectOk(getConfig());
    expect(config.vite_server_url).toBe(VALID_TRIO.vite_server_url);
    expect(config.vite_socket_path).toBe('/from-a-function');
  });

  it('treats a truthy non-object elitea_ui_config as an absent source (§3.6, old code returned no keys for it)', () => {
    g['elitea_ui_config'] = 'bogus-config-string';
    setEnvGlobal({
      VITE_SERVER_URL: 'https://env.example',
      VITE_BASE_URI: '/app/',
      VITE_PUBLIC_PROJECT_ID: '7',
    });

    const config = expectOk(getConfig());
    expect(config.vite_server_url).toBe('https://env.example');
  });

  it('treats a truthy non-object __ENV__ as an absent source (old code THREW here — §3.6 deviation)', () => {
    setWindowConfig({ ...VALID_TRIO });
    g['__ENV__'] = 'bogus';

    expect(expectOk(getConfig()).vite_server_url).toBe(VALID_TRIO.vite_server_url);
  });

  it('skips the process.env source when the process global is absent (browser parity)', () => {
    setWindowConfig(trioWithout('vite_server_url'));
    const result = getConfigWithStubbedProcess(undefined);

    expect(result.status).toBe('missing');
  });

  it('skips the process.env source when process has no env object', () => {
    setWindowConfig(trioWithout('vite_server_url'));
    const result = getConfigWithStubbedProcess({});

    expect(result.status).toBe('missing');
  });
});

describe('frozen results and memoization', () => {
  it('returns a deeply frozen ok result', () => {
    setWindowConfig({ ...VALID_TRIO });

    const result = getConfig();
    const config = expectOk(result);
    expect(Object.isFrozen(result)).toBe(true);
    expect(Object.isFrozen(config)).toBe(true);
    expect(() => {
      (config as unknown as Record<string, unknown>)['vite_server_url'] = 'mutated';
    }).toThrow(TypeError);
  });

  it('returns a deeply frozen missing result', () => {
    const result = getConfig();
    expect(result.status).toBe('missing');
    if (result.status !== 'missing') {
      throw new Error('unreachable');
    }
    expect(Object.isFrozen(result)).toBe(true);
    expect(Object.isFrozen(result.missing)).toBe(true);
    expect(Object.isFrozen(result.reasons)).toBe(true);
    expect(() => {
      (result.missing as unknown as string[]).push('vite_extra');
    }).toThrow(TypeError);
  });

  it('memoizes: repeated calls return the same frozen result for the boot snapshot', () => {
    const windowConfig: Record<string, unknown> = { ...VALID_TRIO };
    setWindowConfig(windowConfig);

    const first = getConfig();
    expect(getConfig()).toBe(first);

    // Sources are immutable for the life of a page (config.js runs before the
    // bundle); a late mutation is therefore NOT observed…
    windowConfig['vite_server_url'] = 'https://mutated.example';
    expect(getConfig()).toBe(first);
    expect(expectOk(first).vite_server_url).toBe(VALID_TRIO.vite_server_url);

    // …until the test-only reset re-resolves from the current sources.
    resetConfigForTests();
    const second = getConfig();
    expect(second).not.toBe(first);
    expect(expectOk(second).vite_server_url).toBe('https://mutated.example');
  });

  it('does not memoize a missing result: a config published later is read (desktop sign-in order)', () => {
    // The desktop shell renders before sign-in and reads the basename
    // (BrandDocumentHead) while no source defines the trio yet…
    expect(getConfig().status).toBe('missing');

    // …then launchApp publishes elitea_ui_config and imports the app.
    setWindowConfig({ ...VALID_TRIO });
    const resolved = getConfig();
    expect(expectOk(resolved).vite_server_url).toBe(VALID_TRIO.vite_server_url);
    expect(getConfig()).toBe(resolved);
  });
});
