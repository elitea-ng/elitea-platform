/**
 * Regression coverage for issue #80 — the AI-configuration `ModelConfiguration`
 * layer.
 *
 * Two defects this file pins, both of which made the capability chips
 * unreachable even after the section was rendered:
 *
 * 1. `getModelCapabilities` matched a group by `group[0].value`, which is a
 *    MODEL id, against `configuration_uid`, which is the OWNING PROJECT
 *    (`useModelConfiguration` sets `configuration_uid: modelInfo.project_id`).
 *    No group ever matched, so the list was always empty.
 * 2. Capabilities were read from a nested `capabilities` map only. The model
 *    catalogue of elitea-main sends one boolean per capability at the top level
 *    of the item and no `capabilities` map at all, so every model on this
 *    platform reported no capability.
 */
import { describe, expect, it } from 'vitest';

import {
  buildConfigurationData,
  getConfigurationOptions,
  getModelCapabilities,
} from './modelConfiguration.helpers';

/** One row as `GET /configurations/models/{projectId}` answers it on elitea-main. */
const CURRENT_PLATFORM_MODEL = {
  name: 'gpt-4o',
  display_name: 'GPT-4o',
  project_id: '7',
  supports_reasoning: true,
  supports_vision: true,
};

/** One row in the legacy shape, which carries an id and a capability map. */
const LEGACY_CAPABILITY_MAP_MODEL = {
  id: '7_gpt-4o',
  name: 'gpt-4o',
  project_id: '7',
  capabilities: { chat_completion: true, function_calling: true, embedding: false },
};

describe('getConfigurationOptions', () => {
  it('groups by owning project and keeps the project as the group id', () => {
    const options = getConfigurationOptions([CURRENT_PLATFORM_MODEL]);
    expect(Object.keys(options)).toEqual(['Project 7']);
    expect(options['Project 7']?.[0]?.group).toBe('7');
    expect(options['Project 7']?.[0]?.value).toBe('gpt-4o');
  });

  it('drops a repeated model instead of offering it twice', () => {
    const options = getConfigurationOptions([CURRENT_PLATFORM_MODEL, CURRENT_PLATFORM_MODEL]);
    expect(options['Project 7']).toHaveLength(1);
  });
});

describe('getModelCapabilities', () => {
  it('matches the group by owning project, not by the first model of the group', () => {
    /* Two models in one project. The group's FIRST entry is `gpt-4o`, and the
       selected model is the SECOND one. Matching on `group[0].value` found no
       group at all; matching on the group id finds it. */
    const options = getConfigurationOptions([
      CURRENT_PLATFORM_MODEL,
      { name: 'o3-mini', project_id: '7', supports_reasoning: true },
    ]);
    expect(getModelCapabilities(options, '7', 'o3-mini')).toEqual(['Reasoning']);
  });

  it('reads the capability booleans elitea-main sends', () => {
    const options = getConfigurationOptions([CURRENT_PLATFORM_MODEL]);
    expect(getModelCapabilities(options, '7', 'gpt-4o')).toEqual(['Reasoning', 'Vision']);
  });

  it('still reads a legacy capability map, and maps the known keys to labels', () => {
    const options = getConfigurationOptions([LEGACY_CAPABILITY_MAP_MODEL]);
    expect(getModelCapabilities(options, '7', 'gpt-4o')).toEqual(['Chat', 'Function Calling']);
  });

  it('compares the two project ids as text, because elitea-main sends a number', () => {
    const options = getConfigurationOptions([{ ...CURRENT_PLATFORM_MODEL, project_id: 7 as unknown as string }]);
    expect(getModelCapabilities(options, 7 as unknown as string, 'gpt-4o')).toEqual(['Reasoning', 'Vision']);
  });

  it('reports nothing for a model with no capability set, so the section stays hidden', () => {
    const options = getConfigurationOptions([{ name: 'plain', project_id: '7' }]);
    expect(getModelCapabilities(options, '7', 'plain')).toEqual([]);
  });

  it('reports nothing while no model is selected', () => {
    expect(getModelCapabilities(getConfigurationOptions([CURRENT_PLATFORM_MODEL]), '7', '')).toEqual([]);
  });
});

describe('buildConfigurationData', () => {
  it('reports the capability keys of the selected model in the copied payload', () => {
    const payload = buildConfigurationData({
      userApiUrl: 'https://elitea.example.test/api/v2',
      projectId: '7',
      model: { model_name: 'gpt-4o', configuration_uid: '7', configuration_name: 'Project 7' },
      configurationsBySections: { llm: [{ id: 1, type: 'openai', data: { name: 'gpt-4o' } }] },
      uniqueConfigurations: [CURRENT_PLATFORM_MODEL],
    });
    expect(payload['model_capabilities']).toEqual(['reasoning', 'vision']);
  });

  it('carries the absolute server and OpenAI base URLs', () => {
    const payload = buildConfigurationData({
      userApiUrl: 'https://elitea.example.test/api/v2',
      projectId: '7',
      model: {},
      configurationsBySections: {},
      uniqueConfigurations: [],
    });
    expect(payload['project_configuration']).toEqual({
      server_url: 'https://elitea.example.test/api/v2',
      base_url: 'https://elitea.example.test/llm/v1',
      project_id: '7',
    });
  });
});
