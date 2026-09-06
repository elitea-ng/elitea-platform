/**
 * The AI-configuration `ModelConfiguration` layer (issue #80).
 *
 * The baseline renders the settings page as three levels:
 * `pages/settings/AIConfiguration.jsx` -> `Configuration/ModelConfiguration.jsx`
 * -> (`ProjectAIConfiguration`, `ConfigurationsPanel`, `ModelCapabilitiesSection`).
 * This app ported the leaves and skipped the middle level, so the capability
 * chips and the copy-configuration button had no host and no caller.
 *
 * This hook is that middle level. It owns three things the page has no other
 * source for: the LLM catalogue, WHICH model the chips describe, and the
 * grouped option map `getModelCapabilities` reads.
 *
 * THE MODEL IS NOW CHOSEN, NOT ONLY AUTO-SELECTED (#80, item 4). The baseline
 * auto-selects the project's default model and its chips describe that one, so
 * a user could not ask "what can THIS model do?" about any other model in the
 * catalogue. `useModelConfiguration` still supplies the initial selection; the
 * `selectedModel`/`onSelectModel` pair below lets the page put a picker in
 * front of the chips, and the chips then follow it.
 *
 * The option list is the baseline's own `useModelOptions`, ported (item 2).
 * `ConfigurationsPanel` reads the same hook for its default-model selects, and
 * both share one react-query cache entry per section.
 */
import { useCallback, useMemo } from 'react';

import { isPublicProject } from '@/entities/project';
import { getConfig } from '@/shared/config';

import type { ModelInfo } from '@/entities/credential';

import {
  buildConfigurationData,
  getConfigurationOptions,
  getModelCapabilities,
} from './modelConfiguration.helpers';
import { useModelConfiguration } from './useModelConfiguration';
import { useModelOptions } from './useModelOptions';

export interface ModelConfigurationLayerParams {
  /** The project the user works in — the project that pays for a `/llm` call. */
  readonly projectId: string;
  /** `shared/config`'s `vite_server_url`, the baseline's `state.user.api_url`. */
  readonly userApiUrl: string;
  /** The section map the page already holds, reused for the copied payload. */
  readonly configurationsBySection: Record<string, unknown[]> | null;
}

export interface ModelConfigurationLayer {
  /** Display labels for `ModelCapabilitiesSection`. Empty hides the section. */
  readonly capabilities: readonly string[];
  /** Writes the whole card as JSON to the clipboard. */
  readonly copyConfiguration: () => void;
  /** The LLM catalogue as `${name}<<>>${project_id}` options — what the picker offers. */
  readonly modelOptions: readonly { readonly value: string; readonly label: string }[];
  /** The picked model in that same value shape; `''` before the catalogue settles. */
  readonly selectedModel: string;
  /** Takes a value from `modelOptions`. An unknown value is ignored, not applied as a blank selection. */
  readonly onSelectModel: (value: string) => void;
}

/**
 * `include_shared: projectId != PUBLIC_PROJECT_ID` — the same test
 * `ConfigurationsPanel` and `OpenAITemplate` apply to the same route, so the
 * three share one react-query cache entry instead of fetching three times.
 */
function includeSharedFor(projectId: string): boolean {
  const result = getConfig();
  if (result.status !== 'ok') return true;
  return !isPublicProject(projectId, result.config.vite_public_project_id);
}

export function useModelConfigurationLayer({
  projectId,
  userApiUrl,
  configurationsBySection,
}: ModelConfigurationLayerParams): ModelConfigurationLayer {
  const includeShared = useMemo(() => includeSharedFor(projectId), [projectId]);

  /* The MODEL CATALOGUE, not the configuration list: only the catalogue
     carries the per-model capability flags and the real `default` flag.
     `useModelOptions` already de-duplicates it and builds the picker's
     options from the same rows, so this level asks for it once. */
  const { uniqueConfigurations, modelOptions } = useModelOptions({ projectId, includeShared });

  /* The initial selection: `useModelConfiguration` auto-selects the project's
     default model, so the chips describe something on first paint. */
  const { model, onChangeModel } = useModelConfiguration({ projectId, configurations: uniqueConfigurations });

  /* `useModelConfiguration` records the owning project as `configuration_uid`
     (`buildModelState`), which is the same half `createOptions` puts after the
     `<<>>`. Building the value from the selection state rather than from a
     second piece of state keeps the picker and the chips reading one source. */
  const selectedModel = model.model_name === '' ? '' : `${model.model_name}<<>>${model.configuration_uid}`;

  const onSelectModel = useCallback(
    (value: string) => {
      const picked = findModelByOptionValue(uniqueConfigurations, value);
      /* An unknown value is ignored. Applying it would blank the chips and
         tell the user the model has no capabilities, which is a different
         claim from "that model is not in this catalogue". */
      if (picked === undefined) return;
      onChangeModel(picked);
    },
    [uniqueConfigurations, onChangeModel],
  );

  const options = useMemo(() => getConfigurationOptions(uniqueConfigurations), [uniqueConfigurations]);

  const capabilities = useMemo(
    () => getModelCapabilities(options, model.configuration_uid, model.model_name),
    [options, model.configuration_uid, model.model_name],
  );

  const copyConfiguration = useCallback(() => {
    const payload = buildConfigurationData({
      userApiUrl,
      projectId,
      model,
      configurationsBySections: (configurationsBySection ?? {}) as Record<string, Array<Record<string, unknown>>>,
      uniqueConfigurations: uniqueConfigurations as unknown as Array<Record<string, unknown>>,
    });
    /* The clipboard write is the whole action. It rejects only when the
       browser denies clipboard access, which leaves nothing to roll back and
       nothing to retry, so the rejection is absorbed — the same posture as
       `OpenAITemplate`'s copy button. */
    void navigator.clipboard?.writeText(JSON.stringify(payload, null, 2)).catch(() => undefined);
  }, [configurationsBySection, model, projectId, uniqueConfigurations, userApiUrl]);

  return { capabilities, copyConfiguration, modelOptions, selectedModel, onSelectModel };
}

/** The inverse of `createOptions`' `${name}<<>>${project_id}` value. */
function findModelByOptionValue(models: readonly ModelInfo[], value: string): ModelInfo | undefined {
  const [name, owningProjectId] = value.split('<<>>');
  return models.find((entry) => entry.name === name && String(entry.project_id) === owningProjectId);
}
