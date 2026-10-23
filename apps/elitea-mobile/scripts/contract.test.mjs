// Tests for scripts/contract.mjs. Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, it } from 'node:test';

import { CLIENT_OPERATIONS, ContractError, SPEC, buildContract, generateTypes, prune } from './contract.mjs';

describe('the spec source', () => {
  it('hashes the operation list, not the file: a $comment edit or a reorder does not stale the client', () => {
    const declared = JSON.parse(readFileSync(CLIENT_OPERATIONS, 'utf8'));
    const dir = mkdtempSync(join(tmpdir(), 'ops-'));
    const other = join(dir, 'client-operations.json');
    writeFileSync(other, JSON.stringify({ $comment: 'edited', operations: [...declared.operations].reverse() }));
    assert.equal(buildContract({ operationsPath: other }).inputs.operations, buildContract().inputs.operations);
    writeFileSync(other, JSON.stringify({ operations: declared.operations.slice(1) }));
    assert.notEqual(buildContract({ operationsPath: other }).inputs.operations, buildContract().inputs.operations);
  });

  it('is the server document in this repository, not a pinned copy', () => {
    assert.match(SPEC.replaceAll('\\', '/'), /services\/elitea-main\/api\/openapi\/v2\.yaml$/);
    assert.ok(readFileSync(SPEC, 'utf8').startsWith('openapi:'));
  });

  it('fails with a reason when the spec is missing', () => {
    assert.throws(() => buildContract({ specPath: '/nonexistent/v2.yaml' }), (error) => error instanceof ContractError && /missing/.test(error.message));
  });

  it('keeps only title and client contract from info, so a server version bump does not churn the client', async () => {
    const { doc, inputs } = buildContract();
    assert.deepEqual(Object.keys(doc.info).sort(), ['title', 'version', 'x-elitea-client-contract']);
    assert.equal(doc.info.version, 'monorepo');
    const source = await generateTypes(doc, inputs);
    assert.ok(source.startsWith('/**\n * GENERATED FILE'));
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
  it('keeps exactly the declared operations', () => {
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

describe('client contract 1.8+ (the spec in this repository)', () => {
  it('speaks client contract 1.8 or later and defines the Local work handoff the thread screen uses', () => {
    const { doc } = buildContract();
    const [major, minor] = String(doc.info['x-elitea-client-contract']).split('.').map(Number);
    assert.ok(major > 1 || (major === 1 && minor >= 8), doc.info['x-elitea-client-contract']);
    const ids = Object.values(doc.paths).flatMap((item) => Object.values(item).map((op) => op?.operationId));
    for (const id of ['getWorkspaceHandoff', 'eraseWorkspaceHandoff', 'requestWorkspaceHandoffReclaim', 'permissionList']) assert.ok(ids.includes(id), id);
    for (const schema of ['ConversationSummary', 'ConversationDetail']) {
      assert.equal(doc.components.schemas[schema].properties.workspace_executor.type, 'string', schema);
    }
  });

  it('defines rename (updateConversation, name alone), pin/unpin and is_pinned upstream, tagged client', () => {
    const { doc } = buildContract();
    const item = doc.paths['/elitea_core/conversation/prompt_lib/{project_id}/{conversation_id}'];
    assert.equal(item.put.operationId, 'updateConversation');
    assert.equal(item.put.tags.at(-1), 'client');
    const body = doc.components.schemas.UpdateConversationRequest;
    assert.equal(item.put.requestBody.content['application/json'].schema.$ref, '#/components/schemas/UpdateConversationRequest');
    assert.equal(body.properties.name.type, 'string');
    assert.equal(body.required, undefined, 'a body with name alone renames');
    assert.ok(item.put.responses['400'] && item.put.responses['404'] && item.put.responses['426']);
    assert.ok(item.get && item.delete, 'the GET and DELETE stay');
    const pin = doc.paths['/social/pin/prompt_lib/{project_id}/{entity_type}/{entity_id}'];
    assert.equal(pin.post.operationId, 'pinEntity');
    assert.equal(pin.delete.operationId, 'unpinEntity');
    assert.equal(pin.post.tags.at(-1), 'client');
    const summary = doc.components.schemas.ConversationSummary;
    assert.equal(summary.properties.is_pinned.type, 'boolean');
  });
});

describe('client contract 1.3 (still served)', () => {
  it('keeps the 1.3 operations the app uses, every one tagged client', () => {
    const { doc } = buildContract();
    const ops = {};
    for (const item of Object.values(doc.paths)) {
      for (const op of Object.values(item)) if (op && op.operationId) ops[op.operationId] = op;
    }
    for (const id of [
      'cancelChatExecution',
      'regenerateChatMessage',
      'getMessageFeedback',
      'setMessageFeedback',
      'deleteMessageFeedback',
      'exportConversation',
      'getRecommendations',
      'deleteNotification',
      'deleteNotifications',
      'listMemories',
      'deleteMemory',
      'clearMemories',
      'getProjectUsage',
    ]) {
      assert.ok(ops[id], id);
      assert.equal(ops[id].tags.at(-1), 'client', id);
    }
    const cancel = doc.paths['/elitea_core/task/prompt_lib/{project_id}/{response_message_id}'].delete;
    assert.ok(cancel.responses['204'] && cancel.responses['409'] && cancel.responses['426']);
    assert.ok(ops.deleteNotification.responses['204']);
    assert.ok(ops.deleteMemory.responses['204']);
    assert.ok(ops.clearMemories.responses['200']);
    for (const schema of ['NativeClientPolicy', 'ClientPublicPolicy']) {
      const props = doc.components.schemas[schema].properties;
      for (const key of ['allow_share_out', 'allow_share_in', 'allow_cloud_stt', 'notification_preview', 'allow_notification_actions', 'allow_system_surfaces']) {
        assert.ok(props[key], `${schema}.${key}`);
      }
    }
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
      'getCurrentAuthor',
    ]) {
      assert.ok(tags[id]?.includes('client'), id);
    }
  });

  it('keeps the 1.2 "who am I" read with its version gate, and the paused trace step', () => {
    const { doc } = buildContract();
    const get = doc.paths['/social/author'].get;
    assert.equal(get.operationId, 'getCurrentAuthor');
    assert.ok(get.responses['426'] && get.responses['400'], 'version-gate responses');
    const author = doc.components.schemas.SocialAuthorProfile;
    assert.ok(author.required.includes('id'));
    assert.equal(author.properties.id.type, 'string');
    const step = doc.components.schemas.MessageTraceStep.properties;
    assert.match(step.finish_reason.description, /awaiting_approval/);
  });
});
