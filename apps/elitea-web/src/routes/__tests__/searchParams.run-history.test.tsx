import { describe } from 'vitest';

import { describeParamCases } from './searchParamsTestKit';

describe('search-params > run-history', () => {
  describeParamCases('run-history', [
    { key: 'history_run_id', valid: 'run-1', malformed: { a: 1 } },
    { key: 'review_delegation', valid: '7', malformed: { a: 1 } },
  ]);
});
