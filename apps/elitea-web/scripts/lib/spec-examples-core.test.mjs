import { describe, expect, it } from 'vitest';

import { checkSpecExamples, collectExampleCases, toJsonSchema } from './spec-examples-core.mjs';

const ok = (spec) => checkSpecExamples(spec).findings;

describe('toJsonSchema', () => {
  it('turns nullable into anyOf-with-null and re-points local refs at the spec', () => {
    expect(toJsonSchema({ type: 'string', nullable: true })).toEqual({ anyOf: [{ type: 'string' }, { type: 'null' }] });
    expect(toJsonSchema({ $ref: '#/components/schemas/A' })).toEqual({ $ref: 'spec#/components/schemas/A' });
    expect(toJsonSchema({ $ref: 'other.yaml#/A' })).toEqual({ $ref: 'other.yaml#/A' });
    expect(toJsonSchema({ allOf: [{ type: 'integer' }], nullable: false })).toEqual({ allOf: [{ type: 'integer' }] });
  });

  it('descends only into schema-valued keywords; literals and property names stay verbatim', () => {
    const schema = {
      type: 'object',
      properties: { nullable: { type: 'boolean' }, $ref: { type: 'string', nullable: true } },
      patternProperties: { '^x-': { $ref: '#/components/schemas/X' } },
      additionalProperties: { type: 'integer' },
      items: [{ type: 'string' }, true],
      not: { type: 'null' },
      enum: [{ nullable: true, $ref: '#/kept' }],
      default: { nullable: true },
    };
    expect(toJsonSchema(schema)).toEqual({
      type: 'object',
      properties: { nullable: { type: 'boolean' }, $ref: { anyOf: [{ type: 'string' }, { type: 'null' }] } },
      patternProperties: { '^x-': { $ref: 'spec#/components/schemas/X' } },
      additionalProperties: { type: 'integer' },
      items: [{ type: 'string' }, true],
      not: { type: 'null' },
      enum: [{ nullable: true, $ref: '#/kept' }],
      default: { nullable: true },
    });
  });

  it('drops a boolean exclusive bound that has no sibling bound', () => {
    expect(toJsonSchema({ type: 'number', exclusiveMinimum: true })).toEqual({ type: 'number' });
  });

  it('rewrites the 3.0 boolean exclusive bounds to draft-07 numbers', () => {
    expect(toJsonSchema({ minimum: 0, exclusiveMinimum: true, maximum: 9, exclusiveMaximum: false })).toEqual({
      exclusiveMinimum: 0,
      maximum: 9,
    });
  });
});

describe('collectExampleCases', () => {
  it('finds examples on media types, parameters, headers and schema nodes, and skips refs', () => {
    const shared = { type: 'string', example: 'shared' };
    const spec = {
      paths: {
        '/a/{id}': {
          parameters: [{ name: 'id', in: 'path', schema: { type: 'integer' }, example: 1 }, null],
          get: {
            parameters: [{ $ref: '#/components/parameters/P' }, { name: 'q', in: 'query', schema: { type: 'string' }, examples: { one: { value: 'x' } } }],
            responses: {
              200: {
                description: 'ok',
                headers: { 'X-H': { schema: { type: 'integer' }, example: 2 } },
                content: {
                  'application/json': {
                    schema: {
                      type: 'object',
                      properties: { a: shared, b: shared },
                      items: { type: 'string', example: 'i' },
                      additionalProperties: true,
                      not: { type: 'null', example: null },
                      allOf: [{ type: 'object' }],
                      oneOf: [{ $ref: '#/components/schemas/S' }],
                    },
                    examples: {
                      named: { value: { a: 'v' } },
                      byRef: { $ref: '#/components/examples/E' },
                      external: { externalValue: 'https://example.test/e.json' },
                      missing: { $ref: '#/components/examples/Nope' },
                    },
                  },
                  'text/plain': { example: 'no schema, nothing to check' },
                },
              },
              404: { $ref: '#/components/responses/R' },
              204: { description: 'no content' },
              500: null,
            },
          },
          post: { requestBody: { content: { 'application/json': { schema: { type: 'integer' }, example: 3 } } } },
          put: { requestBody: { $ref: '#/components/requestBodies/B' } },
          patch: { requestBody: null },
        },
      },
      components: {
        schemas: { S: { type: 'integer', example: 4 } },
        parameters: { P: { name: 'p', in: 'query', schema: { type: 'string' }, example: 'p' } },
        headers: { H: { schema: { type: 'string' }, example: 'h' } },
        responses: { R: { description: 'r', content: { 'application/json': { schema: { type: 'string' }, example: 'r' } } } },
        requestBodies: { B: { content: { 'application/json': { schema: { type: 'boolean' }, example: true } } } },
        examples: { E: { value: { a: 'from ref' } } },
      },
    };
    const pointers = collectExampleCases(spec).map((c) => c.pointer);
    expect(pointers).toEqual([
      '#/paths/~1a~1{id}/parameters/0/example',
      '#/paths/~1a~1{id}/get/parameters/1/examples/one',
      '#/paths/~1a~1{id}/get/responses/200/content/application~1json/schema/properties/a/example',
      '#/paths/~1a~1{id}/get/responses/200/content/application~1json/schema/items/example',
      '#/paths/~1a~1{id}/get/responses/200/content/application~1json/schema/not/example',
      '#/paths/~1a~1{id}/get/responses/200/content/application~1json/examples/named',
      '#/paths/~1a~1{id}/get/responses/200/content/application~1json/examples/byRef',
      '#/paths/~1a~1{id}/get/responses/200/headers/X-H/example',
      '#/paths/~1a~1{id}/post/requestBody/content/application~1json/example',
      '#/components/schemas/S/example',
      '#/components/parameters/P/example',
      '#/components/headers/H/example',
      '#/components/responses/R/content/application~1json/example',
      '#/components/requestBodies/B/content/application~1json/example',
    ]);
  });

  it('accepts a document with neither paths nor components', () => {
    expect(collectExampleCases({})).toEqual([]);
    expect(checkSpecExamples({})).toEqual({ checked: 0, findings: [] });
  });
});

describe('checkSpecExamples', () => {
  const withSchema = (schema, example) => ({
    paths: { '/x': { get: { responses: { 200: { content: { 'application/json': { schema, example } } } } } } },
    components: { schemas: { User: { type: 'object', required: ['id'], properties: { id: { type: 'integer' }, team: { type: 'string', nullable: true } } } } },
  });

  it('passes a valid example that reaches a component through a ref', () => {
    expect(ok(withSchema({ $ref: '#/components/schemas/User' }, { id: 1, team: null }))).toEqual([]);
  });

  it('fails an example that breaks its schema, naming the pointer and the field', () => {
    const findings = ok(withSchema({ $ref: '#/components/schemas/User' }, { id: 'one' }));
    expect(findings).toHaveLength(1);
    expect(findings[0].pointer).toBe('#/paths/~1x/get/responses/200/content/application~1json/example');
    expect(findings[0].message).toMatch(/\/id must be integer/);
  });

  it('reports a root-level mismatch as (root)', () => {
    expect(ok(withSchema({ type: 'array' }, {}))[0].message).toMatch(/\(root\) must be array/);
  });

  it('checks the OpenAPI formats', () => {
    const fmt = (format, type, example) => ok(withSchema({ type, format }, example));
    expect(fmt('int32', 'integer', 2 ** 31 - 1)).toEqual([]);
    expect(fmt('int32', 'integer', 2 ** 31)).toHaveLength(1);
    expect(fmt('int64', 'integer', 2 ** 40)).toEqual([]);
    expect(fmt('int64', 'number', 1.5)).toHaveLength(1);
    expect(fmt('binary', 'string', 'bytes')).toEqual([]);
    expect(fmt('byte', 'string', 'Ynl0ZXM=')).toEqual([]);
    expect(fmt('date-time', 'string', '2026-07-20T00:00:00Z')).toEqual([]);
    expect(fmt('date-time', 'string', 'yesterday')).toHaveLength(1);
  });
});
