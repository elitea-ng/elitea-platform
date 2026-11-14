import { describe } from 'vitest';

import { describeParamCases } from './searchParamsTestKit';

/**
 * The "on any" shell-wide scope (PARAM-062..087, minus `shared_chat` which
 * P1's manifest also routes here despite the name — see `params.json`
 * PARAM-077/078). Composed once into `commonSearchSchema`
 * (`-search/common.ts`), mounted on `_shell/route.tsx` so every wrapped
 * route inherits it.
 */
describe('search-params > shared', () => {
  describeParamCases('shared', [
    { key: 'author_id', valid: 'u-1', malformed: { a: 1 } },
    { key: 'author_name', valid: 'Jane', malformed: { a: 1 } },
    { key: 'bucket', valid: 'my-bucket', malformed: { a: 1 } },
    { key: 'conversation', valid: 'conv-1', malformed: { a: 1 } },
    { key: 'create', valid: '1', malformed: 'yes' },
    { key: 'destTab', valid: 'History', malformed: { a: 1 } },
    { key: 'entity_folder', valid: '12', malformed: { a: 1 } },
    { key: 'from', valid: '/agents', malformed: { a: 1 } },
    { key: 'history_run_id', valid: 'run-1', malformed: { a: 1 } },
    { key: 'index_name', valid: 'idx-1', malformed: { a: 1 } },
    { key: 'isFromCreation', valid: '1', malformed: 'yes' },
    { key: 'message_id', valid: 'msg-1', malformed: { a: 1 } },
    { key: 'name', valid: 'My Item', malformed: { a: 1 } },
    { key: 'project_id', valid: '42', malformed: { a: 1 } },
    { key: 'return_url', valid: '/chat', malformed: { a: 1 } },
    { key: 'save_toolkit', valid: '1', malformed: 'always' },
    { key: 'shared_chat', valid: '1', malformed: 'nope' },
    { key: 'sort_by', valid: 'name', malformed: { a: 1 } },
    { key: 'sort_order', valid: 'asc', malformed: 'sideways' },
    { key: 'statuses', valid: ['active'], malformed: { a: 1 } },
    { key: 'tags[]', valid: ['x'], malformed: { a: 1 } },
    { key: 'toolkit_type', valid: 'github', malformed: { a: 1 } },
    { key: 'tour', valid: 'onboarding-tour', malformed: { a: 1 } },
    { key: 'view', valid: 'grid', malformed: 'circle' },
    { key: 'viewMode', valid: 'owner', malformed: 'admin' },
  ]);
});
