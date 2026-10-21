/**
 * `credentialRequiredFields`: the project credential form reads the schema's
 * `if`/`then`/`else`, so vertex_ai's service-account document stays required
 * in the key mode — the only mode a project credential may use.
 */
import { describe, expect, it } from 'vitest';

import type { ConfigSchemaNode } from '@/features/credentials';

import { credentialRequiredFields } from './CredentialFormSection';

/**
 * The catalogue's vertex_ai `data` schema, as the snapshot states it. Parsed
 * from JSON because an object literal with a `then` key is a lint error.
 */
const VERTEX = JSON.parse(
  `{"required":["vertex_project","vertex_location"],
    "if":{"properties":{"auth_mode":{"const":"workload_identity"}},"required":["auth_mode"]},
    "then":{"not":{"required":["vertex_credentials"]}},
    "else":{"required":["vertex_credentials"]}}`,
) as ConfigSchemaNode;

describe('credentialRequiredFields', () => {
  it('requires the document in the key mode, stated or absent', () => {
    expect(credentialRequiredFields(VERTEX, { auth_mode: 'key' })).toEqual([
      'vertex_project',
      'vertex_location',
      'vertex_credentials',
    ]);
    expect(credentialRequiredFields(VERTEX, {})).toContain('vertex_credentials');
  });

  it('does not require it in the workload identity mode', () => {
    expect(credentialRequiredFields(VERTEX, { auth_mode: 'workload_identity' })).toEqual([
      'vertex_project',
      'vertex_location',
    ]);
  });

  it('applies the stricter else branch to an if it cannot read', () => {
    const odd: ConfigSchemaNode = { ...VERTEX, if: { anyOf: [] } };
    expect(credentialRequiredFields(odd, { auth_mode: 'workload_identity' })).toContain('vertex_credentials');
  });

  it('is the plain required list without a condition', () => {
    expect(credentialRequiredFields({ required: ['api_key'] }, {})).toEqual(['api_key']);
    expect(credentialRequiredFields(undefined, {})).toEqual([]);
  });
});
