/**
 * The nine option lists the AI-providers page's default-model selects need
 * (issue #80, item 2).
 *
 * Port of `apps/elitea-ui/src/[fsd]/features/settings/lib/hooks/
 * useModelConfiguration.hooks.jsx:147-224`'s `useModelOptions`, driven by the
 * same six per-section model queries `AIProvidersContent.jsx:25-72` makes.
 *
 * WHY IT MATTERS, MEASURED — not a tidy-up. `ConfigurationsPanel` built its
 * own options from the CONFIGURATION rows
 * (`GET /configurations/configurations/{projectId}`), labelling each with
 * `elitea_title`. The selects then compare that against the value the same
 * panel computes for the current default, which is
 * `${default_model_name}<<>>${default_model_project_id}` from
 * `GET /configurations/models/{projectId}` — a MODEL NAME. Read-only
 * measurement on the signed-in production tab, `https://next.elitea.ai`,
 * project 1:
 *
 *  - the six LLM option values were `gpt-56-luna<<>>1`, `gpt-56-terra<<>>1`,
 *    `anthropic_claude_5_sonnet<<>>1`, `gpt-54<<>>1`,
 *    `anthropic_claude_46_sonnet<<>>1`, `gpt-54-mini<<>>1`, and the select's
 *    own value was `global.openai.gpt-5.6-luna<<>>1`. NONE of them matched, so
 *    the Default select could not show the model that IS the default. The nine
 *    values this hook builds include that one exactly.
 *  - the catalogue holds NINE models and the project holds SIX configuration
 *    rows, so three models — `gpt-4.1`,
 *    `eu.anthropic.claude-haiku-4-5-20251001-v1:0` and the bedrock
 *    claude-sonnet-4-5 — could not be picked as a default at all.
 *  - saving was wrong in the same way. `useDefaultModelSaving` splits the
 *    option value and POSTs its left half as `name`, so picking an option sent
 *    a configuration title where the server expects a model name.
 *
 * Reading the catalogue instead fixes all three at once, and it is what the
 * baseline always did.
 *
 * NOT a defect this fixes: a section with no configuration rows renders
 * nothing at all (`ConfigurationSection.tsx`'s own early return, and the
 * baseline's `ConfigurationSection.jsx:106` does the same). Production project
 * 1 has no `embedding` and no `vectorstorage` row, so those two sections are
 * absent from the page in both apps. That is the baseline's behaviour, and
 * changing it is not this hook's business.
 *
 * NO EXTRA REQUESTS. Every list here comes from `useModelsQuery`, keyed by
 * `(projectId, section, includeShared)`. `ConfigurationsPanel` already asks
 * for the same six keys to read the defaults, so react-query serves both from
 * one cache entry per section.
 */
import { useMemo } from 'react';

import type { ModelInfo } from '@/entities/credential';

import { EMPTY_MODELS_RESPONSE, useModelsQuery, type ModelsApiResponse } from '../../api/ai-configuration/api';

import { removeDuplicateModels } from './modelConfiguration.helpers';

/** One entry of a default-model select. Not exported — reachable through `ModelOptionsResult`, and knip flags an unused named export. */
interface ModelOption {
  readonly value: string;
  readonly label: string;
}

/**
 * The six sections the page shows a default for. `ai_credentials` is absent on
 * purpose: it has no default model, and the panel renders no select for it.
 */
export interface ModelOptionsResult {
  /** The LLM catalogue, de-duplicated — what the capability chips describe. */
  readonly uniqueConfigurations: readonly ModelInfo[];
  readonly modelOptions: ModelOption[];
  readonly lowTierModelOptions: ModelOption[];
  readonly highTierModelOptions: ModelOption[];
  readonly embeddingModelOptions: ModelOption[];
  readonly vectorStorageOptions: ModelOption[];
  readonly imageGenerationOptions: ModelOption[];
  readonly asrOptions: ModelOption[];
  readonly ttsOptions: ModelOption[];
  /** Each section's response, for the defaults the selects display. */
  readonly sectionData: Readonly<Record<ModelSection, ModelsApiResponse>>;
}

type ModelSection = 'llm' | 'embedding' | 'vectorstorage' | 'image_generation' | 'asr' | 'tts';

/**
 * `${name}<<>>${project_id}` — the value shape `useDefaultModelSaving` splits
 * back into the `name`/`target_project_id` pair the POST takes, and the shape
 * `defaultValueOf` builds from the section's reported default so the select
 * can find its current option by value equality.
 *
 * The baseline's icon (a briefcase for a project-owned model, a share glyph
 * for a shared one) is NOT ported: `SingleSelect` renders a plain string
 * label, and half-plumbing an icon through it would be new surface with no
 * reader. The information is not lost — the shared models are the ones whose
 * `project_id` is the public project.
 */
function createOptions(items: readonly ModelInfo[] | undefined): ModelOption[] {
  return removeDuplicateModels(items === undefined ? [] : [...items]).map((model) => ({
    value: `${model.name}<<>>${String(model.project_id)}`,
    label: model.display_name || model.name,
  }));
}

export interface ModelOptionsInput {
  readonly projectId: string;
  /** `projectId != PUBLIC_PROJECT_ID`, the same test every other reader of this route applies. */
  readonly includeShared: boolean;
}

export function useModelOptions({ projectId, includeShared }: ModelOptionsInput): ModelOptionsResult {
  const llm = useModelsQuery(projectId, 'llm', includeShared).data;
  const embedding = useModelsQuery(projectId, 'embedding', includeShared).data;
  const vectorstorage = useModelsQuery(projectId, 'vectorstorage', includeShared).data;
  const imageGeneration = useModelsQuery(projectId, 'image_generation', includeShared).data;
  const asr = useModelsQuery(projectId, 'asr', includeShared).data;
  const tts = useModelsQuery(projectId, 'tts', includeShared).data;

  const uniqueConfigurations = useMemo(
    () => removeDuplicateModels(llm === undefined ? [] : [...llm.items]),
    [llm],
  );

  return useMemo(
    () => ({
      uniqueConfigurations,
      modelOptions: createOptions(uniqueConfigurations),
      /* The tier flags live on the CATALOGUE row (`low_tier`/`high_tier`,
         confirmed on the production response), so the two tier lists are
         subsets of the same catalogue rather than a second source. */
      lowTierModelOptions: createOptions(uniqueConfigurations.filter((model) => model.low_tier === true)),
      highTierModelOptions: createOptions(uniqueConfigurations.filter((model) => model.high_tier === true)),
      embeddingModelOptions: createOptions(embedding?.items),
      vectorStorageOptions: createOptions(vectorstorage?.items),
      imageGenerationOptions: createOptions(imageGeneration?.items),
      asrOptions: createOptions(asr?.items),
      ttsOptions: createOptions(tts?.items),
      sectionData: {
        llm: withDefaults(llm),
        embedding: withDefaults(embedding),
        vectorstorage: withDefaults(vectorstorage),
        image_generation: withDefaults(imageGeneration),
        asr: withDefaults(asr),
        tts: withDefaults(tts),
      },
    }),
    [uniqueConfigurations, embedding, vectorstorage, imageGeneration, asr, tts, llm],
  );
}

/**
 * `useModelsQuery` resolves to `undefined` before the fetch settles. A
 * top-level helper rather than a `??` at each of the six call sites, for the
 * §3.5 complexity budget.
 */
function withDefaults(data: ModelsApiResponse | undefined): ModelsApiResponse {
  return data ?? EMPTY_MODELS_RESPONSE;
}
