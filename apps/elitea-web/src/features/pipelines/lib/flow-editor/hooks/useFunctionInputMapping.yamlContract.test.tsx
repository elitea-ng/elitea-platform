import { waitFor } from '@testing-library/react';
import { load } from 'js-yaml';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { PIPELINE_165_YAML } from '../../../__tests__/pipeline165Fixture';
import { renderWithRouterAndProject } from '../../../__tests__/testUtils';
import { serializePipelineYaml } from '../../dumpYaml.helpers';
import { parsePipelineYamlDocument } from '../../pipelineYamlDocument.helpers';
import type { YamlPipelineDocument } from '../helpers/pipelineFlow.types';
import type { UseFunctionInputMappingArgs, UseFunctionInputMappingResult } from './useFunctionInputMapping';
import { useFunctionInputMapping } from './useFunctionInputMapping';

const BASE = '/api/v2';
const PROJECT_ID = 'proj-1';

const nullableString = { anyOf: [{ type: 'string' }, { type: 'null' }], default: null };
const nullableList = { anyOf: [{ items: { type: 'string' }, type: 'array' }, { type: 'null' }], default: null };

/** The artifact type's argument schemas for the two tools, as Main serves them (descriptions and titles trimmed). */
const ARTIFACT_SCHEMAS = {
  artifact: {
    properties: {
      selected_tools: {
        args_schemas: {
          create_file: {
            properties: { bucket_name: nullableString, filedata: nullableString, filename: { type: 'string' }, filepath: nullableString },
            required: ['filename'],
          },
          list_files: {
            properties: {
              bucket_name: nullableString,
              folder: nullableString,
              include: nullableList,
              recursive: { anyOf: [{ type: 'boolean' }, { type: 'null' }], default: false },
              skip: nullableList,
            },
          },
        },
      },
    },
  },
};

/** The attached artifact toolkit `test`: no credentials yet, no explicit `selected_tools`. */
const VERSION_TOOLS = [{ type: 'artifact', name: 'test', toolkit_name: 'test' }];

function HookProbe({ onResult, ...args }: UseFunctionInputMappingArgs & { onResult: (result: UseFunctionInputMappingResult) => void }) {
  onResult(useFunctionInputMapping(args));
  return null;
}

function renderNode(id: string, setYamlJsonObject: (next: YamlPipelineDocument) => void): () => UseFunctionInputMappingResult | undefined {
  const { yamlJsonObject } = parsePipelineYamlDocument(PIPELINE_165_YAML);
  let latest: UseFunctionInputMappingResult | undefined;
  renderWithRouterAndProject(
    <HookProbe
      id={id}
      yamlJsonObject={yamlJsonObject}
      setYamlJsonObject={setYamlJsonObject}
      versionTools={VERSION_TOOLS}
      onResult={(result) => {
        latest = result;
      }}
    />,
    PROJECT_ID,
  );
  return () => latest;
}

/** Every document the editor hands on must serialize and load back as the very same value. */
function expectEveryWriteRoundTrips(setYamlJsonObject: ReturnType<typeof vi.fn>): void {
  for (const [written] of setYamlJsonObject.mock.calls as [YamlPipelineDocument][]) {
    expect(load(serializePipelineYaml(written))).toStrictEqual(written);
  }
}

describe('toolkit node load path keeps the pipeline YAML contract (pipeline 165)', () => {
  beforeEach(() => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json(ARTIFACT_SCHEMAS)));
    server.use(http.post(`${BASE}/elitea_core/toolkit_discover_tools/prompt_lib/${PROJECT_ID}/:toolkitType`, () => HttpResponse.json({ tools: [], total: 0 })));
  });

  afterEach(() => {
    resetGeneratedClient();
  });

  it('fills an empty input_mapping with defaults that the strict serializer round-trips unchanged', async () => {
    const setYamlJsonObject = vi.fn();
    renderNode('ls', setYamlJsonObject);

    // What the editor builds: `input_mapping: {}` gains the tool's defaults on load (baseline behaviour).
    await waitFor(() =>
      expect((setYamlJsonObject.mock.calls.at(-1)?.[0] as YamlPipelineDocument | undefined)?.nodes?.find(node => node.id === 'ls')?.input_mapping).toStrictEqual({
        bucket_name: { type: 'fixed', value: null },
        folder: { type: 'fixed', value: null },
        include: { type: 'fixed', value: null },
        recursive: { type: 'fixed', value: false },
        skip: { type: 'fixed', value: null },
      }),
    );
    // What it passes to `serializePipelineYaml`.
    expectEveryWriteRoundTrips(setYamlJsonObject);
  });

  it('keeps a node whose mapping already covers its required inputs serializable', async () => {
    const setYamlJsonObject = vi.fn();
    const read = renderNode('mk', setYamlJsonObject);

    // The schema has landed once `filename` is known to be required; the default-mapping effect has run with it.
    await waitFor(() => expect(read()?.requiredInputs).toEqual(['filename']));
    await waitFor(() => expect(read()?.inputMappings['bucket_name']).toStrictEqual({ type: 'fixed', value: null }));
    // `filename` is mapped already, so nothing is written for `mk`, and nothing unserializable could be.
    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });
});
