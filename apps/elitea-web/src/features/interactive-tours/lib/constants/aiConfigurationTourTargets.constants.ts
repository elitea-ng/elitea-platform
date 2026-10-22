/**
 * Port of `apps/elitea-ui/src/[fsd]/features/interactive-tours/lib/constants/aiConfigurationTourTargets.constants.js`
 */

import { buildTourSelector } from '../helpers/tourSelector.helpers';

export const AI_CONFIG_TOUR_TARGET_IDS = {
  page: 'ai-config-page',
  serverConfig: 'ai-config-server-config',
  integrations: 'ai-config-integrations',
  llmModels: 'ai-config-llm-models',
  embeddingModels: 'ai-config-embedding-models',
  imageGeneration: 'ai-config-image-generation',
  aiCredentials: 'ai-config-ai-credentials',
  openaiTemplateTab: 'ai-config-openai-template-tab',
};

export const AI_CONFIG_TOUR_TARGETS = {
  page: buildTourSelector(AI_CONFIG_TOUR_TARGET_IDS.page),
  serverConfig: buildTourSelector(AI_CONFIG_TOUR_TARGET_IDS.serverConfig),
  integrations: buildTourSelector(AI_CONFIG_TOUR_TARGET_IDS.integrations),
  llmModels: buildTourSelector(AI_CONFIG_TOUR_TARGET_IDS.llmModels),
  embeddingModels: buildTourSelector(AI_CONFIG_TOUR_TARGET_IDS.embeddingModels),
  imageGeneration: buildTourSelector(AI_CONFIG_TOUR_TARGET_IDS.imageGeneration),
  aiCredentials: buildTourSelector(AI_CONFIG_TOUR_TARGET_IDS.aiCredentials),
  openaiTemplateTab: buildTourSelector(AI_CONFIG_TOUR_TARGET_IDS.openaiTemplateTab),
};
