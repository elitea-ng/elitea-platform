/**
 * The LIVE lanes' environment contract — the one place that says which real
 * credential each ported legacy journey needs, and how to build the platform
 * payloads out of it.
 *
 * ## Why a module and not five copies in five specs
 *
 * `playwright.config.ts` has to answer the same question the specs do —
 * "is this provider configured on this machine?" — because the two live
 * projects are LISTED only when the answer is yes (see the config's own
 * `LIVE_*` block). A second copy of the rule in the config would drift from
 * the specs, and the drift would show up as a project that lists a spec whose
 * credential is absent: a red run that says nothing about the product.
 *
 * NOTHING HERE IMPORTS `playwright.config.ts`. The config imports this file,
 * so the reverse edge would be a cycle at module load, and Playwright loads
 * the config before anything else.
 *
 * ## The rule these lanes obey
 *
 * A provider whose REQUIRED variables are all present and non-empty is
 * configured. A configured provider's spec runs; an unconfigured provider's
 * spec is not listed at all. There is no `test.skip` anywhere in `e2e/live`
 * — a skip reads as coverage in the report, and the whole point of these
 * lanes is that the coverage is real when it is claimed. `npx playwright test
 * --list` with no live variables set must show ZERO tests in both live
 * projects, which is the assertion `scripts/e2e-journey-shape.test.mjs`
 * makes on this file and on the config together.
 *
 * ## Ported from
 *
 * `qa/elitea-testing-public/automation/toolkit_configs.py` (`TOOLKIT_CONFIGS`)
 * and `automation/toolkit_factories.py` — the legacy suite's own registry of
 * per-provider payloads, tools and prompts. The names differ: the legacy
 * suite read bare `GITHUB_TOKEN`/`JIRA_API_KEY`/… out of a `.env.test`, and
 * every variable here carries the `E2E_LIVE_` prefix so that a developer's
 * ordinary shell (which very often already exports `GITHUB_TOKEN`) cannot
 * arm a lane that talks to a real provider by accident.
 */

/** Read one variable, treating an empty string as absent. */
function env(name: string): string {
  const value = process.env[name];
  return value === undefined ? '' : value.trim();
}

/** Read one variable, falling back to `fallback` when it is absent. */
function envOr(name: string, fallback: string): string {
  const value = env(name);
  return value === '' ? fallback : value;
}

/** The five toolkit types the legacy suite parametrises over. */
export const LIVE_TOOLKIT_IDS = ['github', 'jira', 'gitlab', 'bitbucket', 'confluence'] as const;

export type LiveToolkitId = (typeof LIVE_TOOLKIT_IDS)[number];

export interface LiveToolkitProvider {
  readonly id: LiveToolkitId;
  /** The `config_schema.title` the credential chooser labels the type with. */
  readonly displayName: string;
  /** Every variable that must be set, or the provider is not configured. */
  readonly requiredEnv: readonly string[];
  /** Variables with a working default; listed for the README and the CI job. */
  readonly optionalEnv: readonly string[];
  /** The credential `data` object, built from the REAL secret. */
  credentialData(): Readonly<Record<string, string>>;
  /**
   * The same object with the SECRET replaced by a value the provider will
   * reject. Everything else — the base URL, the user name — stays real, so
   * the refusal that comes back is an authentication refusal and not a
   * "cannot resolve host".
   */
  brokenCredentialData(): Readonly<Record<string, string>>;
  /** The toolkit `settings` object, referencing a saved credential. */
  toolkitSettings(eliteaTitle: string): Readonly<Record<string, unknown>>;
  /** The tool the legacy Test-Settings case selected, by its runtime name. */
  readonly probeTool: string;
  /** A prompt that names `probeTool`, for the tool-run journey. */
  readonly probePrompt: string;
  /**
   * A string only the REAL provider can put in the answer.
   *
   * This is what replaces the legacy Test-Settings panel's raw-result read
   * (`test_tool_result_content`, e.g. `"main"` in the JSON the GitHub tool
   * returned). It must NOT appear anywhere in `probePrompt`, or a model that
   * never called the tool could satisfy it by repeating the question — the
   * `liveEnv` unit test asserts exactly that for every provider.
   */
  probeEvidence(): string;
  /** The legacy `chat_message` — natural language, names no tool. */
  readonly chatPrompt: string;
  /**
   * The legacy `chat_response_keywords`. The answer must carry at least one,
   * lower-cased. Kept semantic on purpose: a real model writes its own
   * sentence and an exact-text assertion here would be a flake by
   * construction — the same rule `chat-stream-real`'s own note states.
   */
  readonly answerKeywords: readonly string[];
}

/** A token no provider will accept, and that names itself in a log. */
const WRONG_SECRET = 'autotest-deliberately-invalid-token';

export const LIVE_TOOLKIT_PROVIDERS: Readonly<Record<LiveToolkitId, LiveToolkitProvider>> = {
  github: {
    id: 'github',
    displayName: 'GitHub',
    requiredEnv: ['E2E_LIVE_GITHUB_TOKEN', 'E2E_LIVE_GITHUB_REPOSITORY'],
    optionalEnv: ['E2E_LIVE_GITHUB_BASE_URL', 'E2E_LIVE_GITHUB_BRANCH'],
    credentialData: () => ({
      base_url: envOr('E2E_LIVE_GITHUB_BASE_URL', 'https://api.github.com'),
      access_token: env('E2E_LIVE_GITHUB_TOKEN'),
    }),
    brokenCredentialData: () => ({
      base_url: envOr('E2E_LIVE_GITHUB_BASE_URL', 'https://api.github.com'),
      access_token: WRONG_SECRET,
    }),
    toolkitSettings: (eliteaTitle) => ({
      github_configuration: { elitea_title: eliteaTitle, private: true },
      repository: env('E2E_LIVE_GITHUB_REPOSITORY'),
      active_branch: envOr('E2E_LIVE_GITHUB_BRANCH', 'main'),
      base_branch: envOr('E2E_LIVE_GITHUB_BRANCH', 'main'),
      selected_tools: ['list_branches_in_repo'],
    }),
    probeTool: 'list_branches_in_repo',
    probePrompt: 'Use the list_branches_in_repo tool and report every branch name it returns.',
    probeEvidence: () => envOr('E2E_LIVE_GITHUB_BRANCH', 'main'),
    chatPrompt: 'List branches in the repository',
    answerKeywords: ['branch', 'repository'],
  },

  jira: {
    id: 'jira',
    displayName: 'Jira',
    requiredEnv: [
      'E2E_LIVE_JIRA_BASE_URL',
      'E2E_LIVE_JIRA_USERNAME',
      'E2E_LIVE_JIRA_API_KEY',
      'E2E_LIVE_JIRA_PROJECT_KEY',
    ],
    optionalEnv: [],
    credentialData: () => ({
      base_url: env('E2E_LIVE_JIRA_BASE_URL'),
      username: env('E2E_LIVE_JIRA_USERNAME'),
      api_key: env('E2E_LIVE_JIRA_API_KEY'),
    }),
    brokenCredentialData: () => ({
      base_url: env('E2E_LIVE_JIRA_BASE_URL'),
      username: env('E2E_LIVE_JIRA_USERNAME'),
      api_key: WRONG_SECRET,
    }),
    toolkitSettings: (eliteaTitle) => ({
      jira_configuration: { elitea_title: eliteaTitle, private: true },
      cloud: true,
      limit: 50,
      api_version: '3',
      verify_ssl: true,
      // Explicit, for the reason the legacy factory states in its own comment:
      // the tool list is read from `settings.selected_tools` when it is there,
      // so the probe tool is offered without a catalogue round trip.
      selected_tools: ['list_projects'],
    }),
    probeTool: 'list_projects',
    probePrompt: 'Use the list_projects tool and report the project keys it returns.',
    probeEvidence: () => env('E2E_LIVE_JIRA_PROJECT_KEY'),
    chatPrompt: 'List all Jira projects',
    answerKeywords: ['project', 'jira'],
  },

  gitlab: {
    id: 'gitlab',
    displayName: 'GitLab',
    requiredEnv: ['E2E_LIVE_GITLAB_PRIVATE_TOKEN', 'E2E_LIVE_GITLAB_REPOSITORY'],
    optionalEnv: ['E2E_LIVE_GITLAB_URL', 'E2E_LIVE_GITLAB_BRANCH'],
    credentialData: () => ({
      url: envOr('E2E_LIVE_GITLAB_URL', 'https://gitlab.com'),
      private_token: env('E2E_LIVE_GITLAB_PRIVATE_TOKEN'),
    }),
    brokenCredentialData: () => ({
      url: envOr('E2E_LIVE_GITLAB_URL', 'https://gitlab.com'),
      private_token: WRONG_SECRET,
    }),
    toolkitSettings: (eliteaTitle) => ({
      gitlab_configuration: { elitea_title: eliteaTitle, private: true },
      repository: env('E2E_LIVE_GITLAB_REPOSITORY'),
      branch: envOr('E2E_LIVE_GITLAB_BRANCH', 'main'),
      selected_tools: ['list_branches_in_repo'],
    }),
    probeTool: 'list_branches_in_repo',
    probePrompt: 'Use the list_branches_in_repo tool and report every branch name it returns.',
    probeEvidence: () => envOr('E2E_LIVE_GITLAB_BRANCH', 'main'),
    chatPrompt: 'List branches in the repository',
    answerKeywords: ['branch', 'repository'],
  },

  bitbucket: {
    id: 'bitbucket',
    displayName: 'Bitbucket',
    requiredEnv: [
      'E2E_LIVE_BITBUCKET_USERNAME',
      'E2E_LIVE_BITBUCKET_TOKEN',
      'E2E_LIVE_BITBUCKET_PROJECT',
      'E2E_LIVE_BITBUCKET_REPOSITORY',
    ],
    optionalEnv: ['E2E_LIVE_BITBUCKET_URL', 'E2E_LIVE_BITBUCKET_BRANCH'],
    credentialData: () => ({
      url: envOr('E2E_LIVE_BITBUCKET_URL', 'https://api.bitbucket.org'),
      username: env('E2E_LIVE_BITBUCKET_USERNAME'),
      password: env('E2E_LIVE_BITBUCKET_TOKEN'),
    }),
    brokenCredentialData: () => ({
      url: envOr('E2E_LIVE_BITBUCKET_URL', 'https://api.bitbucket.org'),
      username: env('E2E_LIVE_BITBUCKET_USERNAME'),
      password: WRONG_SECRET,
    }),
    toolkitSettings: (eliteaTitle) => ({
      bitbucket_configuration: { elitea_title: eliteaTitle, private: true },
      project: env('E2E_LIVE_BITBUCKET_PROJECT'),
      repository: env('E2E_LIVE_BITBUCKET_REPOSITORY'),
      branch: envOr('E2E_LIVE_BITBUCKET_BRANCH', 'master'),
      selected_tools: ['list_branches_in_repo'],
    }),
    probeTool: 'list_branches_in_repo',
    probePrompt: 'Use the list_branches_in_repo tool and report every branch name it returns.',
    probeEvidence: () => envOr('E2E_LIVE_BITBUCKET_BRANCH', 'master'),
    chatPrompt: 'List branches in the repository',
    answerKeywords: ['branch', 'repository'],
  },

  confluence: {
    id: 'confluence',
    displayName: 'Confluence',
    requiredEnv: [
      'E2E_LIVE_CONFLUENCE_BASE_URL',
      'E2E_LIVE_CONFLUENCE_USERNAME',
      'E2E_LIVE_CONFLUENCE_API_KEY',
      'E2E_LIVE_CONFLUENCE_SPACE',
    ],
    optionalEnv: ['E2E_LIVE_CONFLUENCE_LABEL'],
    credentialData: () => ({
      base_url: env('E2E_LIVE_CONFLUENCE_BASE_URL'),
      username: env('E2E_LIVE_CONFLUENCE_USERNAME'),
      api_key: env('E2E_LIVE_CONFLUENCE_API_KEY'),
    }),
    brokenCredentialData: () => ({
      base_url: env('E2E_LIVE_CONFLUENCE_BASE_URL'),
      username: env('E2E_LIVE_CONFLUENCE_USERNAME'),
      api_key: WRONG_SECRET,
    }),
    toolkitSettings: (eliteaTitle) => ({
      confluence_configuration: { elitea_title: eliteaTitle, private: true },
      space: env('E2E_LIVE_CONFLUENCE_SPACE'),
      cloud: true,
      limit: 50,
      selected_tools: ['list_pages_with_label'],
    }),
    probeTool: 'list_pages_with_label',
    probePrompt: `Use the list_pages_with_label tool with label '${envOr('E2E_LIVE_CONFLUENCE_LABEL', 'test')}' and report the space key of every page it returns.`,
    probeEvidence: () => env('E2E_LIVE_CONFLUENCE_SPACE'),
    chatPrompt: `Use the list_pages_with_label tool to list pages with label '${envOr('E2E_LIVE_CONFLUENCE_LABEL', 'test')}' in Confluence`,
    answerKeywords: ['page', 'label'],
  },
};

/** Is every required variable of this provider set and non-empty? */
export function isLiveToolkitConfigured(id: LiveToolkitId): boolean {
  return LIVE_TOOLKIT_PROVIDERS[id].requiredEnv.every((name) => env(name) !== '');
}

/** The providers this machine can drive, in registry order. */
export function configuredLiveToolkits(): readonly LiveToolkitId[] {
  return LIVE_TOOLKIT_IDS.filter(isLiveToolkitConfigured);
}

/**
 * The model that serves the image lane, or `''`.
 *
 * `E2E_LIVE_IMAGE_MODEL` is the model name AS THE PICKER OFFERS IT — the
 * `provider/model` row the stack's own catalogue carries — because that is
 * what the journey selects. The legacy suite hard-coded "GPT-5.2"; naming it
 * here instead is what lets one lane serve whatever image-capable provider a
 * given deployment is configured with.
 */
export function liveImageModel(): string {
  return env('E2E_LIVE_IMAGE_MODEL');
}

/** Every variable either lane reads, for the README and the CI job to print. */
export function liveEnvNames(): readonly string[] {
  const names = new Set<string>(['E2E_LIVE_IMAGE_MODEL']);
  for (const id of LIVE_TOOLKIT_IDS) {
    for (const name of LIVE_TOOLKIT_PROVIDERS[id].requiredEnv) names.add(name);
    for (const name of LIVE_TOOLKIT_PROVIDERS[id].optionalEnv) names.add(name);
  }
  return [...names].sort();
}
