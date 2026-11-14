/**
 * indexExecution.helpers.test.ts — issue 310's admission helpers.
 */
import { describe, expect, it } from 'vitest';

import { EliteaApiError } from '@/shared/api/generated/mutator';

import {
  ACTIVE_INDEX_CONFLICT_MESSAGE,
  canStartToolkitRun,
  isBoundedIndexExecutionTaskId,
  isFrameForCurrentIndexExecution,
  parseIndexStartConflictTaskId,
  parseIndexStartRefusalMessage,
} from './indexExecution.helpers';

describe('isBoundedIndexExecutionTaskId', () => {
  it('accepts a normal task id', () => {
    expect(isBoundedIndexExecutionTaskId('0123456789abcdef0123456789abcdef')).toBe(true);
  });

  it('rejects a non-string, empty, or whitespace-only value', () => {
    expect(isBoundedIndexExecutionTaskId(undefined)).toBe(false);
    expect(isBoundedIndexExecutionTaskId(null)).toBe(false);
    expect(isBoundedIndexExecutionTaskId(42)).toBe(false);
    expect(isBoundedIndexExecutionTaskId('')).toBe(false);
    expect(isBoundedIndexExecutionTaskId(' ')).toBe(false);
    expect(isBoundedIndexExecutionTaskId(' task-1')).toBe(false);
    expect(isBoundedIndexExecutionTaskId('task-1 ')).toBe(false);
  });

  it('rejects a value carrying NUL, CR, or LF (before it could ever reach a URL)', () => {
    expect(isBoundedIndexExecutionTaskId('task\r\n2')).toBe(false);
    expect(isBoundedIndexExecutionTaskId('task\x002')).toBe(false);
  });

  it('rejects a value over 512 bytes, accepts exactly 512', () => {
    expect(isBoundedIndexExecutionTaskId('x'.repeat(512))).toBe(true);
    expect(isBoundedIndexExecutionTaskId('x'.repeat(513))).toBe(false);
  });
});

describe('parseIndexStartConflictTaskId', () => {
  function conflictError(body: unknown, status = 409): EliteaApiError {
    return new EliteaApiError({ kind: 'http', status, url: '/elitea_core/test_toolkit_tool/prompt_lib/7', body });
  }

  it('adopts the task id from the exact current-start conflict shape', () => {
    expect(
      parseIndexStartConflictTaskId(
        conflictError({ error: ACTIVE_INDEX_CONFLICT_MESSAGE, task_id: 'existing-task-1' }),
      ),
    ).toBe('existing-task-1');
  });

  it('rejects a non-EliteaApiError value', () => {
    expect(parseIndexStartConflictTaskId(new Error('boom'))).toBeUndefined();
    expect(parseIndexStartConflictTaskId('boom')).toBeUndefined();
    expect(parseIndexStartConflictTaskId(undefined)).toBeUndefined();
  });

  it('rejects a non-409 status, even with the exact conflict body', () => {
    expect(
      parseIndexStartConflictTaskId(conflictError({ error: ACTIVE_INDEX_CONFLICT_MESSAGE, task_id: 'task-1' }, 403)),
    ).toBeUndefined();
  });

  it('rejects an auth-kind failure (no body to trust, even at 409)', () => {
    const error = new EliteaApiError({ kind: 'auth', status: 409, url: '/x' });
    expect(parseIndexStartConflictTaskId(error)).toBeUndefined();
  });

  it('rejects a generic conflict message', () => {
    expect(parseIndexStartConflictTaskId(conflictError({ error: 'already active', task_id: 'task-1' }))).toBeUndefined();
  });

  it('rejects a body carrying an unexpected extra field', () => {
    expect(
      parseIndexStartConflictTaskId(
        conflictError({ error: ACTIVE_INDEX_CONFLICT_MESSAGE, task_id: 'task-1', project_id: 7 }),
      ),
    ).toBeUndefined();
  });

  it('rejects a missing or blank task id', () => {
    expect(parseIndexStartConflictTaskId(conflictError({ error: ACTIVE_INDEX_CONFLICT_MESSAGE }))).toBeUndefined();
    expect(
      parseIndexStartConflictTaskId(conflictError({ error: ACTIVE_INDEX_CONFLICT_MESSAGE, task_id: ' ' })),
    ).toBeUndefined();
  });

  it('rejects a task id carrying control characters or over the 512-byte bound', () => {
    expect(
      parseIndexStartConflictTaskId(
        conflictError({ error: ACTIVE_INDEX_CONFLICT_MESSAGE, task_id: 'task\r\n2' }),
      ),
    ).toBeUndefined();
    expect(
      parseIndexStartConflictTaskId(
        conflictError({ error: ACTIVE_INDEX_CONFLICT_MESSAGE, task_id: 'x'.repeat(513) }),
      ),
    ).toBeUndefined();
  });

  it('rejects a body that is not a plain object (array, primitive)', () => {
    expect(parseIndexStartConflictTaskId(conflictError(['error', 'task_id']))).toBeUndefined();
    expect(parseIndexStartConflictTaskId(conflictError('Indexing is already in progress'))).toBeUndefined();
    expect(parseIndexStartConflictTaskId(conflictError(null))).toBeUndefined();
  });
});

describe('canStartToolkitRun', () => {
  const ready = { indexing: true, isCreateIndexMode: false, isValidForm: true, isRunning: false, isIndexing: false };

  it('allows a fresh index run', () => {
    expect(canStartToolkitRun(ready)).toBe(true);
  });

  it('blocks a start while the SAME run is already in flight locally', () => {
    expect(canStartToolkitRun({ ...ready, isRunning: true })).toBe(false);
  });

  it('blocks a start while the index is already active per server metadata (issue 310)', () => {
    expect(canStartToolkitRun({ ...ready, isIndexing: true })).toBe(false);
  });

  it('the isIndexing gate only applies to an indexing run, not a plain tool test', () => {
    expect(canStartToolkitRun({ ...ready, indexing: false, isIndexing: true })).toBe(true);
  });

  it('requires a valid form for a non-indexing run', () => {
    expect(canStartToolkitRun({ ...ready, indexing: false, isValidForm: false })).toBe(false);
  });

  it('indexing bypasses the form-validity requirement outside create-index mode', () => {
    expect(canStartToolkitRun({ ...ready, isValidForm: false })).toBe(true);
  });

  it('indexing REQUIRES form validity while in create-index mode', () => {
    expect(canStartToolkitRun({ ...ready, isCreateIndexMode: true, isValidForm: false })).toBe(false);
    expect(canStartToolkitRun({ ...ready, isCreateIndexMode: true, isValidForm: true })).toBe(true);
  });
});

describe('isFrameForCurrentIndexExecution', () => {
  it('accepts the first frame regardless of its message_id (nothing tracked yet)', () => {
    expect(isFrameForCurrentIndexExecution('m1', undefined)).toBe(true);
    expect(isFrameForCurrentIndexExecution(undefined, undefined)).toBe(true);
  });

  it('accepts a later frame that matches the tracked message_id', () => {
    expect(isFrameForCurrentIndexExecution('m1', 'm1')).toBe(true);
  });

  it('accepts a frame with no message_id at all — nothing to correlate against', () => {
    expect(isFrameForCurrentIndexExecution(undefined, 'm1')).toBe(true);
    expect(isFrameForCurrentIndexExecution('', 'm1')).toBe(true);
  });

  it('rejects a frame naming a DIFFERENT run once one is tracked (issue 310: no message_id guard)', () => {
    expect(isFrameForCurrentIndexExecution('m2', 'm1')).toBe(false);
  });
});

describe('parseIndexStartRefusalMessage (#2042)', () => {
  function refusal(body: unknown, status: number): EliteaApiError {
    return new EliteaApiError({ kind: 'http', status, url: '/elitea_core/test_toolkit_tool/prompt_lib/7', body });
  }

  it('returns the sentence of a 422 or 503 refusal', () => {
    expect(parseIndexStartRefusalMessage(refusal({ error: 'No embedding model is available to this project.' }, 422))).toBe(
      'No embedding model is available to this project.',
    );
    expect(parseIndexStartRefusalMessage(refusal({ error: 'Embedding model is unavailable' }, 503))).toBe('Embedding model is unavailable');
  });

  it('is not fooled by the validation error list, the busy 503 or other statuses', () => {
    expect(parseIndexStartRefusalMessage(refusal({ error: [{ type: 'value_error', loc: ['body'], msg: 'bad' }] }, 422))).toBeUndefined();
    expect(parseIndexStartRefusalMessage(refusal({ error: 'temporarily_unavailable', message: 'busy', retry_after: 3 }, 503))).toBeUndefined();
    expect(parseIndexStartRefusalMessage(refusal({ error: 'Toolkit not found' }, 404))).toBeUndefined();
    expect(parseIndexStartRefusalMessage(refusal({ error: 'x'.repeat(401) }, 422))).toBeUndefined();
    expect(parseIndexStartRefusalMessage(new Error('boom'))).toBeUndefined();
  });
});
