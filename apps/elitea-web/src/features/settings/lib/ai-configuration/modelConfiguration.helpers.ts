/**
 * Model configuration helpers for the AI Configuration feature.
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/lib/helpers/modelConfiguration.helpers.js`.
 */
import { toAbsoluteApiUrl, toOpenAiBaseUrl } from '@/shared/lib/api-url';

/** One entry of the grouped option map that `getConfigurationOptions` builds. */
type ConfigOption = {
  label: string;
  value: string;
  group: string;
  group_name: string;
  capabilities: Record<string, boolean>;
  originalModel?: Record<string, unknown>;
};

/**
 * One row of `GET /configurations/models/{projectId}`.
 *
 * Every field is optional because the two backends do not send the same set.
 * The legacy runtime sends `id` and a nested `capabilities` map. elitea-main
 * sends neither: its catalogue item carries `name`, `display_name`,
 * `project_id`, `shared`, `default` and one boolean per capability
 * (`services/elitea-main/internal/application/configurations/models.go`).
 */
type CatalogueModel = {
  id?: string;
  name?: string;
  display_name?: string;
  project_id?: string;
  capabilities?: Record<string, boolean>;
  supports_reasoning?: boolean;
  supports_vision?: boolean;
};

export const removeDuplicateModels = <T extends { id?: string; name?: string; project_id?: string }>(
  models: T[] | undefined,
): T[] => {
  if (!models) return [];
  const seen = new Set<string>();
  return models.filter((model) => {
    const key = `${model?.id || 'unknown'}-${model?.name || 'unknown'}-${model?.project_id || 'default'}`;
    if (seen.has(key)) {
      return false;
    }
    seen.add(key);
    return true;
  });
};

/**
 * The capability flags of one catalogue row, in the legacy `capabilities`
 * shape.
 *
 * elitea-main DOES NOT SEND `capabilities`. It sends one boolean per capability
 * at the top level of the item. Reading only `capabilities` therefore gave every
 * model an empty flag set on this platform, so the chips were always absent.
 * Read the legacy map when it is present, and fall back to the booleans this
 * platform sends. A model with no capability set keeps an all-false map, which
 * the filters below drop, so the section stays hidden.
 */
function toCapabilityMap(model: CatalogueModel): Record<string, boolean> {
  if (model.capabilities && typeof model.capabilities === 'object') {
    return model.capabilities;
  }
  return {
    reasoning: model.supports_reasoning === true,
    vision: model.supports_vision === true,
  };
}

function optionLabelOf(model: CatalogueModel): string {
  return model?.name || `Model ${model?.id || 'Unknown'}`;
}

function optionValueOf(model: CatalogueModel): string {
  return model?.id || model?.name || 'unknown';
}

function optionGroupOf(model: CatalogueModel): string {
  return model?.project_id || 'default';
}

/**
 * Groups the model catalogue by owning project, one option per model.
 * Ported from the baseline helper of the same name (`ModelConfiguration.jsx`
 * feeds its result to `getModelCapabilities`).
 */
export const getConfigurationOptions = (
  configurations: readonly CatalogueModel[] | undefined,
): Record<string, ConfigOption[]> => {
  const grouped: Record<string, ConfigOption[]> = {};
  for (const model of configurations ?? []) {
    const label = optionLabelOf(model);
    const value = optionValueOf(model);
    const groupName = `Project ${optionGroupOf(model)}`;
    const group = grouped[groupName] ?? [];
    grouped[groupName] = group;
    if (group.some((item) => item.value === value || item.label === label)) {
      continue;
    }
    group.push({
      label,
      value,
      group: optionGroupOf(model),
      group_name: groupName,
      capabilities: toCapabilityMap(model),
      originalModel: model,
    });
  }
  return grouped;
};

/**
 * The raw capability keys of the selected model.
 *
 * MATCH THE GROUP, NOT THE VALUE. A group holds one option per MODEL and the
 * group id is the OWNING PROJECT (`getConfigurationOptions` above, and
 * `useModelConfiguration`'s `configuration_uid: modelInfo.project_id`). This
 * used to compare `group[0].value` — a model name — against the project id, so
 * no group ever matched and the capability list was always empty.
 *
 * `project_id` arrives as a JSON number from elitea-main and as text elsewhere,
 * so compare the two ids as text.
 */
function capabilityKeysOf(
  options: Record<string, ConfigOption[]>,
  configurationUid: string,
  modelName: string,
): string[] {
  const groups = Object.values(options).filter((group) => group.length > 0);
  const found = groups.find((group) => String(group[0]?.group) === String(configurationUid));
  /* `value` is the model id where the backend sends one and the model name
     where it does not. Accept the label too, so a payload that carries both
     still matches the `model_name` the selection state holds. */
  const option = found?.find((item) => item.value === modelName || item.label === modelName);
  return Object.entries(option?.capabilities ?? {})
    .filter(([, enabled]) => enabled)
    .map(([key]) => key);
}

export const getModelCapabilities = (
  options: Record<string, ConfigOption[]>,
  configurationUid: string,
  modelName: string,
): string[] => {
  if (!modelName) return [];

  const capabilityMap: Record<string, string> = {
    chat_completion: 'Chat',
    completion: 'Completion',
    embedding: 'Embeddings',
    embeddings: 'Embeddings',
    reasoning: 'Reasoning',
    code_generation: 'Code Generation',
    function_calling: 'Function Calling',
  };

  return capabilityKeysOf(options, configurationUid, modelName).map(
    (key) => capabilityMap[key] || key.replace(/_/g, ' ').replace(/\b\w/g, (l) => l.toUpperCase()),
  );
};

// ---------------------------------------------------------------------------
// extractConfigName — extracted from buildConfigurationData
// ---------------------------------------------------------------------------

/**
 * Returns the first non-empty string value from the given keys,
 * falling back to the top-level keys on the config object.
 */
function firstNonEmpty(config: Record<string, unknown>, keys: string[]): string {
  for (const key of keys) {
    const val = config[key] as string | undefined;
    if (val) return val;
  }
  return '';
}

/**
 * Extracts a human-readable name from a configuration object,
 * checking multiple possible field names in priority order.
 */
const extractConfigName = (config: Record<string, unknown>): string => {
  const d = config.data as Record<string, unknown> | undefined;
  const cfgSettings = config.settings as Record<string, unknown> | undefined;
  const cfgConfig = config.config as Record<string, unknown> | undefined;
  const pick = (obj: Record<string, unknown> | undefined, key: string): string | undefined =>
    obj?.[key] as string | undefined;
  const keys = ['name', 'model', 'model_name', 'title', 'label', 'elitea_title', 'type'];
  const top = firstNonEmpty(d ?? config, keys);
  return top
    || pick(cfgSettings, 'title')
    || pick(cfgConfig, 'name')
    || '';
};

// ---------------------------------------------------------------------------
// mapConfigToList — extracted from buildConfigurationData
// ---------------------------------------------------------------------------

/**
 * Maps a section's configurations to a flat list of summary objects.
 */
const mapConfigToList = (
  section: Array<Record<string, unknown>> | undefined,
): Array<{
  id: unknown;
  name: string;
  type: unknown;
  shared: boolean;
  project_id: unknown;
}> => {
  return (section || []).map((config) => ({
    id: config.id,
    name: extractConfigName(config),
    type: config.type,
    shared: config.shared === true,
    project_id: config.project_id,
  }));
};

// ---------------------------------------------------------------------------
// buildConfigurationData — orchestrator (complexity ≤ 8)
// ---------------------------------------------------------------------------

/**
 * Builds a data object for display from model and configuration data.
 */
export const buildConfigurationData = ({
  userApiUrl,
  projectId,
  model,
  configurationsBySections,
  uniqueConfigurations,
}: {
  userApiUrl: string;
  projectId: string;
  model: Record<string, unknown>;
  configurationsBySections: Record<string, Array<Record<string, unknown>>>;
  uniqueConfigurations: Array<Record<string, unknown>>;
}): Record<string, unknown> => {
  let capabilitiesList: string[] = [];

  if (uniqueConfigurations && model.model_name && model.configuration_uid) {
    /* The SAME option map the chips read. It used to be a second, private
       builder that grouped raw configuration rows by `configuration_uid` and
       set `value` to that same uid, so it never matched a model name and the
       copied payload always reported an empty `model_capabilities`. */
    capabilitiesList = capabilityKeysOf(
      getConfigurationOptions(uniqueConfigurations),
      model.configuration_uid as string,
      model.model_name as string,
    );
  }

  return {
    project_configuration: {
      // Absolute, not relative: these two land inside a generated SDK snippet
      // the user copies out of the browser, where a bare `/api/v2` path
      // addresses nothing. See `shared/lib/api-url.ts`.
      server_url: userApiUrl ? toAbsoluteApiUrl(userApiUrl) : 'Not configured',
      base_url: userApiUrl ? toOpenAiBaseUrl(userApiUrl) : 'Not configured',
      project_id: projectId || 'Not configured',
    },
    configuration_options: {
      model_name: (model.model_name as string) || '',
      configuration_type: (model.configuration_name as string) || '',
      configuration_uid: (model.configuration_uid as string) || '',
    },
    model_capabilities: capabilitiesList,
    available_configurations: {
      llm_models: mapConfigToList(configurationsBySections.llm),
      embedding_models: mapConfigToList(configurationsBySections.embedding),
      ai_credentials: mapConfigToList(configurationsBySections.ai_credentials),
      image_generation_models: mapConfigToList(configurationsBySections.image_generation),
      asr_models: mapConfigToList(configurationsBySections.asr),
    },
  };
};
