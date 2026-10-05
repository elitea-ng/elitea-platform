// Tests for scripts/contract.mjs. Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, it } from 'node:test';
import YAML from 'yaml';

import {
  ContractError,
  UPSTREAM_SPEC,
  buildContract,
  mergeOverlay,
  parseShaFile,
  prune,
  sha256Hex,
  verifyPin,
} from './contract.mjs';

describe('verifyPin', () => {
  it('accepts the committed upstream copy', () => {
    assert.match(verifyPin(), /^[0-9a-f]{64}$/);
  });

  it('fails on a single-byte change', () => {
    const dir = mkdtempSync(join(tmpdir(), 'contract-'));
    const bytes = readFileSync(UPSTREAM_SPEC);
    const spec = join(dir, 'spec.yaml');
    const sha = join(dir, 'spec.yaml.sha256');
    writeFileSync(sha, `${sha256Hex(bytes)}  spec.yaml\n`);
    writeFileSync(spec, bytes);
    verifyPin(spec, sha);

    const tampered = Buffer.from(bytes);
    tampered[tampered.length - 2] ^= 0x01;
    writeFileSync(spec, tampered);
    assert.throws(() => verifyPin(spec, sha), ContractError);
  });

  it('rejects a malformed sha256 file', () => {
    assert.throws(() => parseShaFile('not a hash'), ContractError);
  });
});

const upstreamFixture = () => ({
  openapi: '3.0.3',
  info: { title: 'fixture', version: '1' },
  servers: [{ url: '/api/v2' }],
  security: [{ BearerAuth: [] }],
  tags: [{ name: 'a' }, { name: 'unused' }],
  paths: {
    '/things/{id}': {
      parameters: [{ $ref: '#/components/parameters/Id' }],
      get: {
        operationId: 'getThing',
        tags: ['a'],
        responses: {
          200: {
            description: 'ok',
            content: { 'application/json': { schema: { $ref: '#/components/schemas/Thing' } } },
          },
          404: { $ref: '#/components/responses/404' },
        },
      },
    },
    '/other': {
      get: {
        operationId: 'getOther',
        tags: ['unused'],
        responses: { 200: { description: 'ok', content: { 'application/json': { schema: { $ref: '#/components/schemas/Other' } } } } },
      },
    },
  },
  components: {
    securitySchemes: { BearerAuth: { type: 'http', scheme: 'bearer' } },
    parameters: { Id: { name: 'id', in: 'path', required: true, schema: { type: 'string' } } },
    responses: {
      404: { description: 'nf', content: { 'application/json': { schema: { $ref: '#/components/schemas/Err' } } } },
    },
    schemas: {
      Thing: { type: 'object', properties: { part: { $ref: '#/components/schemas/Part' } } },
      Part: { type: 'object', properties: { leaf: { type: 'array', items: { $ref: '#/components/schemas/Leaf' } } } },
      Leaf: { type: 'string' },
      Err: { type: 'object', properties: { error: { type: 'string' } } },
      Other: { type: 'object', properties: { x: { $ref: '#/components/schemas/Unreached' } } },
      Unreached: { type: 'string' },
    },
  },
});

const overlayOp = (operationId) => ({
  operationId,
  'x-zefir-source': 'observed: fixture',
  'x-zefir-until': 'ADR-0025 WP5',
  responses: { 204: { description: 'gone' } },
});

describe('mergeOverlay', () => {
  it('adds a new method to an existing upstream path', () => {
    const merged = mergeOverlay(upstreamFixture(), { paths: { '/things/{id}': { delete: overlayOp('deleteThing') } } });
    assert.equal(merged.paths['/things/{id}'].delete.operationId, 'deleteThing');
    assert.equal(merged.paths['/things/{id}'].get.operationId, 'getThing');
  });

  it('fails when upstream already defines the overlay operation', () => {
    assert.throws(
      () => mergeOverlay(upstreamFixture(), { paths: { '/things/{id}': { get: overlayOp('getThing2') } } }),
      (error) => error instanceof ContractError && /upstream now defines GET \/things\/\{id\}/.test(error.message),
    );
  });

  it('fails on a component name collision', () => {
    assert.throws(
      () => mergeOverlay(upstreamFixture(), { components: { schemas: { Thing: { type: 'string' } } } }),
      ContractError,
    );
  });

  it('requires provenance on every overlay operation', () => {
    const op = overlayOp('newOp');
    delete op['x-zefir-source'];
    assert.throws(() => mergeOverlay(upstreamFixture(), { paths: { '/new': { post: op } } }), /x-zefir-source/);
  });

  it('rejects a duplicate operationId', () => {
    assert.throws(
      () => mergeOverlay(upstreamFixture(), { paths: { '/new': { post: overlayOp('getThing') } } }),
      /operationId getThing/,
    );
  });

  it('never mutates its inputs', () => {
    const upstream = upstreamFixture();
    const before = JSON.stringify(upstream);
    mergeOverlay(upstream, { paths: { '/new': { post: overlayOp('newOp') } } });
    assert.equal(JSON.stringify(upstream), before);
  });
});

const extension = (body) => ({
  'x-zefir-source': 'observed: fixture',
  'x-zefir-until': 'ADR-0025 WP5',
  ...body,
});

describe('x-zefir-extend', () => {
  it('adds parameters, responses and properties to upstream definitions', () => {
    const merged = mergeOverlay(upstreamFixture(), {
      'x-zefir-extend': {
        operations: {
          getThing: extension({
            parameters: [{ name: 'since', in: 'query', schema: { type: 'string' } }],
            responses: { 410: { description: 'gone' } },
          }),
        },
        schemas: { Thing: extension({ properties: { cursor: { type: 'string' } } }) },
      },
    });
    const op = merged.paths['/things/{id}'].get;
    assert.deepEqual(op.parameters, [{ name: 'since', in: 'query', schema: { type: 'string' } }]);
    assert.deepEqual(op.responses[410], { description: 'gone' });
    assert.deepEqual(merged.components.schemas.Thing.properties.cursor, { type: 'string' });
  });

  it('fails once upstream defines the same parameter, status or property', () => {
    const upstream = upstreamFixture();
    upstream.paths['/things/{id}'].get.parameters = [{ name: 'since', in: 'query' }];
    assert.throws(
      () =>
        mergeOverlay(upstream, {
          'x-zefir-extend': {
            operations: { getThing: extension({ parameters: [{ name: 'since', in: 'query' }] }) },
          },
        }),
      /upstream now defines query parameter since on getThing/,
    );
    assert.throws(
      () =>
        mergeOverlay(upstreamFixture(), {
          'x-zefir-extend': { operations: { getThing: extension({ responses: { 404: { description: 'x' } } }) } },
        }),
      /upstream now defines response 404 on getThing/,
    );
    assert.throws(
      () =>
        mergeOverlay(upstreamFixture(), {
          'x-zefir-extend': { schemas: { Thing: extension({ properties: { part: { type: 'string' } } }) } },
        }),
      /upstream now defines Thing.part/,
    );
  });

  it('only extends upstream definitions, with provenance and known keys', () => {
    assert.throws(
      () => mergeOverlay(upstreamFixture(), { 'x-zefir-extend': { operations: { nope: extension({}) } } }),
      /target operation nope does not exist/,
    );
    assert.throws(
      () =>
        mergeOverlay(upstreamFixture(), {
          paths: { '/new': { post: overlayOp('newOp') } },
          'x-zefir-extend': { operations: { newOp: extension({}) } },
        }),
      /target operation newOp does not exist/,
    );
    assert.throws(
      () => mergeOverlay(upstreamFixture(), { 'x-zefir-extend': { schemas: { Thing: { properties: {} } } } }),
      /missing x-zefir-source/,
    );
    assert.throws(
      () =>
        mergeOverlay(upstreamFixture(), {
          'x-zefir-extend': { operations: { getThing: extension({ requestBody: {} }) } },
        }),
      /may not set "requestBody"/,
    );
  });

  it('never mutates its inputs', () => {
    const upstream = upstreamFixture();
    const before = JSON.stringify(upstream);
    mergeOverlay(upstream, {
      'x-zefir-extend': { schemas: { Thing: extension({ properties: { cursor: { type: 'string' } } }) } },
    });
    assert.equal(JSON.stringify(upstream), before);
  });
});

describe('prune', () => {
  it('keeps the transitive $ref closure of the allowlisted operations only', () => {
    const out = prune(upstreamFixture(), ['getThing']);
    assert.deepEqual(Object.keys(out.paths), ['/things/{id}']);
    assert.deepEqual(Object.keys(out.components.schemas), ['Err', 'Leaf', 'Part', 'Thing']);
    assert.deepEqual(Object.keys(out.components.parameters), ['Id']);
    assert.deepEqual(Object.keys(out.components.responses), ['404']);
    assert.deepEqual(Object.keys(out.components.securitySchemes), ['BearerAuth']);
    assert.deepEqual(out.tags, [{ name: 'a' }]);
  });

  it('fails on an unknown operationId', () => {
    assert.throws(() => prune(upstreamFixture(), ['nope']), /unknown operationIds: nope/);
  });

  it('fails on a dangling $ref', () => {
    const doc = upstreamFixture();
    delete doc.components.schemas.Leaf;
    assert.throws(() => prune(doc, ['getThing']), /dangling \$ref/);
  });
});

describe('buildContract (real inputs)', () => {
  it('merges the overlay and keeps exactly the declared operations', () => {
    const { doc } = buildContract();
    const ids = [];
    for (const item of Object.values(doc.paths)) {
      for (const op of Object.values(item)) if (op && op.operationId) ids.push(op.operationId);
    }
    const declared = JSON.parse(readFileSync(new URL('../contract/client-operations.json', import.meta.url))).operations;
    assert.deepEqual(ids.sort(), [...declared].sort());
  });

  it('describes changes_since and the delta fields on the three synced lists', () => {
    const { doc } = buildContract();
    const ops = {};
    for (const item of Object.values(doc.paths)) {
      for (const op of Object.values(item)) if (op && op.operationId) ops[op.operationId] = op;
    }
    for (const id of ['listConversations', 'listConversationMessages', 'listNotifications']) {
      assert.ok(ops[id].parameters.some((p) => p.name === 'changes_since' && p.in === 'query'), id);
      assert.ok(ops[id].responses['410'], id);
    }
    for (const schema of ['ConversationListing', 'ConversationMessageListing', 'NotificationListing']) {
      const props = doc.components.schemas[schema].properties;
      assert.ok(props.tombstones && props.next_cursor && props.has_more, schema);
    }
    assert.deepEqual(doc.components.schemas.SyncTombstone.properties.reason.enum, ['deleted', 'access_lost']);
  });
});

describe('client contract 1.1 (pinned upstream)', () => {
  it('pins a server that speaks client contract 1.1 and needs no overlay', () => {
    const { doc } = buildContract();
    assert.equal(doc.info['x-elitea-client-contract'], '1.1');
    const overlay = YAML.parse(readFileSync(new URL('../contract/overlay/native-gap.yaml', import.meta.url), 'utf8'));
    assert.deepEqual(overlay.paths, {});
    assert.equal(overlay['x-zefir-extend'], undefined);
  });

  it('keeps the chat enrichment operations, every one tagged client', () => {
    const { doc } = buildContract();
    const tags = {};
    for (const item of Object.values(doc.paths)) {
      for (const op of Object.values(item)) if (op && op.operationId) tags[op.operationId] = op.tags ?? [];
    }
    for (const id of [
      'sendChatMessage',
      'continueChatExecution',
      'uploadConversationAttachment',
      'downloadConversationAttachment',
      'addConversationParticipants',
      'deleteConversationParticipant',
      'listParticipantCandidates',
      'listApplicationSkills',
      'listToolkitInstances',
      'listToolkitAvailableTools',
      'listMessageTraces',
      'getMessageTrace',
    ]) {
      assert.ok(tags[id]?.includes('client'), id);
    }
  });
});
