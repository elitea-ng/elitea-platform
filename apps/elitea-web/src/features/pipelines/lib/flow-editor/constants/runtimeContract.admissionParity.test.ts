import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { describe, expect, it } from 'vitest';

import { CompilerAdmittedNodeTypes, isCompilerAdmittedNodeType } from './runtimeContract.constants';

/**
 * Node-type admission parity. `libs/proto/contracts/pipeline-node-admission-v1.json`
 * is the one list of the node `type:` values each layer admits; the Worker compiler
 * test and Main's save-time test assert the same file. A node type added to one layer
 * only fails the others; there is no recorded drift (Wave 2a-3 admitted `map`).
 */
interface AdmissionContract {
  readonly admitted: readonly string[];
  readonly refused: readonly string[];
}

const CONTRACT_PATH = resolve(import.meta.dirname, '../../../../../../../../libs/proto/contracts/pipeline-node-admission-v1.json');
const contract = JSON.parse(readFileSync(CONTRACT_PATH, 'utf8')) as AdmissionContract;

describe('the editor admits exactly the node types of the shared admission contract', () => {
  it('matches the admitted set', () => {
    expect([...CompilerAdmittedNodeTypes].sort()).toEqual([...contract.admitted].sort());
  });

  it('refuses every contract-refused type', () => {
    for (const type of contract.refused) expect(isCompilerAdmittedNodeType(type)).toBe(false);
  });
});
