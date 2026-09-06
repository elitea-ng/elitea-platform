/**
 * Configuration helpers for the AI Configuration feature.
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/lib/helpers/configuration.helpers.js`
 * and `apps/elitea-ui/src/[fsd]/features/settings/lib/constants/configuration.constants.js`.
 */

const ICON_TYPE_KEYS: Record<string, readonly string[]> = {
  VERTEX_AI: ['vertex_ai', 'vertexai'],
  AI_DIAL: ['ai_dial', 'dial'],
  OPEN_AI: ['open_ai', 'openai', 'gpt', 'codex mini', 'embedding-ada', 'whisper'],
  CLAUDE: ['claude', 'anthropic', 'opus', 'haiku'],
  OLLAMA: ['ollama'],
  AMAZON_BEDROCK: ['amazon_bedrock'],
  AMAZON: ['amazon.titan'],
  HUGGING_FACE: ['hugging_face', 'huggingface'],
  CHROMA: ['chroma'],
  AZURE: ['open_ai_azure', 'azure', 'azure_openai', 'azure_open_ai', 'model-router'],
  PGVECTOR: ['pgvector', 'postgresql', 'postgres'],
};

const CONFIGURATION_TYPE_GROUPS = {
  OpenAI: {
    label: 'OpenAI',
    types: ['open_ai', 'openai', 'gpt', 'codex mini', 'embedding-ada'],
  },
  Anthropic: {
    label: 'Anthropic',
    types: ['claude', 'anthropic', 'opus', 'haiku'],
  },
  OtherLLMProviders: {
    label: 'Other LLM Providers',
    types: [
      'vertex_ai',
      'vertexai',
      'ai_dial',
      'dial',
      'ollama',
      'amazon_bedrock',
      'amazon.titan',
      'hugging_face',
      'huggingface',
      'chroma',
      'open_ai_azure',
      'azure',
      'azure_openai',
      'azure_open_ai',
      'model-router',
      'pgvector',
      'postgresql',
      'postgres',
    ],
  },
} as const;

const ICON_TYPE_KEYS_ARRAY: readonly { key: string; values: readonly string[] }[] = Object.entries(ICON_TYPE_KEYS)
  .map(([key, values]) => ({ key, values }))
  .sort((a, b) => b.values.length - a.values.length);

const THIRD_PARTY_HOSTING_KEYWORDS = [
  'azure',
  'bedrock',
  'vertex',
  'vertexai',
  'dial',
  'ai_dial',
  'ollama',
  'hugging',
  'model-router',
  'postgres',
];

const OPENAI_GROUP_TYPES = CONFIGURATION_TYPE_GROUPS.OpenAI.types;
const ANTHROPIC_GROUP_TYPES = CONFIGURATION_TYPE_GROUPS.Anthropic.types;
const OTHER_GROUP_LABEL = CONFIGURATION_TYPE_GROUPS.OtherLLMProviders.label;

/**
 * Checks if a key/label string matches any keyword in a group's type list.
 */
function matchesGroupTypes(
  text: string,
  types: readonly string[],
): boolean {
  return types.some(
    (t) => t.toLowerCase() === text || text.includes(t.toLowerCase()),
  );
}

/**
 * Checks if a key/label indicates third-party hosting.
 */
function isThirdPartyHosted(configKey: string, labelKey: string): boolean {
  return THIRD_PARTY_HOSTING_KEYWORDS.some(
    (kw) => configKey.includes(kw) || labelKey.includes(kw),
  );
}

export const getIconTypeKey = (name: string | undefined, type: string | undefined, label: string | undefined): string => {
  const iconKey = (name || type || '').toLowerCase();

  for (const { key, values } of ICON_TYPE_KEYS_ARRAY) {
    if (values.includes(iconKey)) return key;
    if (label && values.some((keyword) => label.toLowerCase().includes(keyword))) return key;
  }

  return 'DEFAULT';
};

const getCfgData = (cfg: Record<string, unknown>): Record<string, unknown> | undefined => {
  return cfg.data as Record<string, unknown> | undefined;
};

/**
 * Checks if a value is a non-empty string.
 */
function isNonEmptyString(value: unknown): boolean {
  if (typeof value !== 'string') return false;
  return value.trim().length > 0;
}

/**
 * Extracts the first non-empty string among a fixed, ordered list of
 * candidate fields. Mirrors the old app's literal `||` chain
 * (`configuration.helpers.js:40-52`) field-for-field, so `data.*` fields
 * are checked interleaved with top-level ones in the *exact* priority order
 * the baseline used — not "all top-level keys, then all nested keys" (that
 * reordering was the Finding-8 regression this replaces).
 */
const firstNonEmptyOf = (...candidates: unknown[]): string | undefined => {
  for (const candidate of candidates) {
    if (isNonEmptyString(candidate)) return String(candidate);
  }
  return undefined;
};

export const getConfigurationDisplayName = (configuration: Record<string, unknown>): string => {
  const data = getCfgData(configuration);
  const settings = configuration.settings as Record<string, unknown> | undefined;
  const config = configuration.config as Record<string, unknown> | undefined;
  const metadata = configuration.metadata as Record<string, unknown> | undefined;

  // Priority order ported verbatim from configuration.helpers.js:40-52:
  // label, data.name, data.model, data.model_name, title, settings.title,
  // config.name, elitea_title, name, metadata.title, metadata.name.
  const rawName = firstNonEmptyOf(
    configuration.label,
    data?.name,
    data?.model,
    data?.model_name,
    configuration.title,
    settings?.title,
    config?.name,
    configuration.elitea_title,
    configuration.name,
    metadata?.title,
    metadata?.name,
  );

  if (rawName && rawName.trim().length > 0) {
    return rawName;
  }

  if (configuration.type) {
    return (configuration.type as string)
      .split('_')
      .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
      .join(' ');
  }

  return 'Unnamed Configuration';
};

export const getConfigurationStatus = (statusOk: boolean, isShared: boolean): string => {
  const status = statusOk ? 'OK' : 'In Progress';
  const scope = isShared ? 'Shared' : 'Local';
  return `${status} \u2022 ${scope}`;
};

/**
 * Normalises a project id for comparison.
 *
 * The two ends of this comparison have different JSON types, and both are
 * correct in their own place: the row's `project_id` arrives from the server
 * as a NUMBER on one route and as a STRING on another (the two Go DTOs in
 * `services/elitea-main/internal/api/v2/configurations/` disagreed —
 * `CurrentConfigurationDTO.ProjectID` is `int32`, the prototype
 * `Configuration.ProjectID` was `string`), while the selected project id is
 * always a string in this app. Normalising both sides to text is the only
 * form that survives either.
 *
 * Only a string or a finite number is an id. Everything else — `null`,
 * `undefined`, an object, `NaN` — normalises to `''`, and an empty id never
 * matches, so an absent or malformed id stays non-editable rather than
 * matching another absent one.
 */
const normaliseProjectId = (value: unknown): string => {
  if (typeof value === 'string') return value.trim();
  if (typeof value === 'number' && Number.isFinite(value)) return String(value);
  return '';
};

const sameProjectId = (left: unknown, right: unknown): boolean => {
  const leftId = normaliseProjectId(left);
  return leftId !== '' && leftId === normaliseProjectId(right);
};

/**
 * DEFECT this fixes: the comparison was `configuration.project_id ===
 * projectId` with the row's id cast to `string | undefined`. The cast was a
 * lie — the list route answers `"project_id": 2`, a number — so `2 === '2'`
 * was false for EVERY row, every card reported "No edit permissions", and no
 * AI configuration in the project could be opened for editing.
 */
export const isConfigurationEditable = (configuration: Record<string, unknown>, projectId: string, canEdit: boolean): boolean => {
  if (!sameProjectId(configuration.project_id, projectId)) return false;
  return canEdit;
};

export const getConfigurationGroup = (
  name: string | undefined,
  type: string | undefined,
  label: string | undefined,
): string => {
  const configKey = (name || type || '').toLowerCase();
  const labelKey = (label || '').toLowerCase();

  if (isThirdPartyHosted(configKey, labelKey)) {
    return OTHER_GROUP_LABEL;
  }

  if (matchesGroupTypes(configKey, OPENAI_GROUP_TYPES)) {
    return CONFIGURATION_TYPE_GROUPS.OpenAI.label;
  }
  if (labelKey && matchesGroupTypes(labelKey, OPENAI_GROUP_TYPES)) {
    return CONFIGURATION_TYPE_GROUPS.OpenAI.label;
  }

  if (matchesGroupTypes(configKey, ANTHROPIC_GROUP_TYPES)) {
    return CONFIGURATION_TYPE_GROUPS.Anthropic.label;
  }
  if (labelKey && matchesGroupTypes(labelKey, ANTHROPIC_GROUP_TYPES)) {
    return CONFIGURATION_TYPE_GROUPS.Anthropic.label;
  }

  return OTHER_GROUP_LABEL;
};

export const sortConfigurationsByDisplayName = (
  a: Record<string, unknown>,
  b: Record<string, unknown>,
): number => {
  const nameA = getConfigurationDisplayName(a).toLowerCase();
  const nameB = getConfigurationDisplayName(b).toLowerCase();
  return nameA.localeCompare(nameB);
};

