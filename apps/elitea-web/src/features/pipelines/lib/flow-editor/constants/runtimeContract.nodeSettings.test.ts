import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { describe, expect, it } from 'vitest';

import { parsePipelineYamlDocument } from '../../pipelineYamlDocument.helpers';
import { collectGraphAdmissionIssues } from '../../graphAdmission.helpers';
import type { YamlPipelineDocument } from '../helpers/pipelineFlow.types';
import {
  EXECUTED_ONCE_KEY, NODE_ERRORS_KEY, NODE_ON_ERROR, NODE_SETTINGS_KEYS, NodeSettingsFamilies, NodeSettingsLimits,
  NodeSettingsPauseFamilies, NodeSettingsRefusalTexts, nodeSettingsMessage, type NodeSettingsRefusal,
} from '../../nodeSettings.helpers';
import { isReservedStateKey } from './runtimeContract.constants';

/**
 * Node settings parity (5b-1). `libs/proto/contracts/pipeline-node-settings-v1.json`
 * is the one source of the settings keys, bounds, family table and refusal texts;
 * the Worker compiler and Main's save check assert the same file, and all three
 * layers run the shared vectors with the same verdicts.
 */
const CONTRACTS = resolve(import.meta.dirname, '../../../../../../../../libs/proto/contracts');
const contract = JSON.parse(readFileSync(resolve(CONTRACTS, 'pipeline-node-settings-v1.json'), 'utf8')) as {
  node_errors_key: string;
  executed_once_key: string;
  keys: string[];
  on_error: string[];
  limits: Record<string, number>;
  families: Record<string, string[]>;
  pause_families: string[];
  refusals: Record<string, string>;
};
const vectors = JSON.parse(readFileSync(resolve(CONTRACTS, 'pipeline-node-settings-v1-vectors.json'), 'utf8')) as {
  cases: { name: string; yaml: string; refusal: NodeSettingsRefusal | null; node: string | null }[];
};

describe('node settings contract', () => {
  it('mirrors the constants, keys, families and refusal texts', () => {
    expect(NodeSettingsLimits).toEqual(contract.limits);
    expect(NODE_ERRORS_KEY).toBe(contract.node_errors_key);
    expect(EXECUTED_ONCE_KEY).toBe(contract.executed_once_key);
    expect([...NODE_SETTINGS_KEYS]).toEqual(contract.keys);
    expect([...NODE_ON_ERROR]).toEqual(contract.on_error);
    expect(NodeSettingsFamilies).toEqual(contract.families);
    expect(NodeSettingsPauseFamilies).toEqual(contract.pause_families);
    expect(NodeSettingsRefusalTexts).toEqual(contract.refusals);
  });

  it('reserves the run-once channel and never node_errors (D2)', () => {
    expect(isReservedStateKey(EXECUTED_ONCE_KEY)).toBe(true);
    expect(isReservedStateKey(NODE_ERRORS_KEY)).toBe(false);
  });

  it.each(vectors.cases.map((vector) => [vector.name, vector] as const))('vector: %s', (_name, vector) => {
    const document = parsePipelineYamlDocument(vector.yaml).yamlJsonObject as YamlPipelineDocument;
    const issues = collectGraphAdmissionIssues(document).filter((issue) => issue.severity !== 'warning');
    const settings = issues.filter((issue) => issue.rule === 'node.settings');
    if (vector.refusal === null) {
      expect(issues).toEqual([]);
      return;
    }
    expect(settings).toHaveLength(1);
    expect(settings[0]!.subject).toBe(vector.refusal);
    expect(settings[0]!.nodeId).toBe(vector.node ?? undefined);
    expect(settings[0]!.message).toBe(nodeSettingsMessage(vector.refusal, vector.node ?? undefined));
  });

  it('holds at least the forty shared vectors', () => {
    expect(vectors.cases.length).toBeGreaterThanOrEqual(40);
  });
});
