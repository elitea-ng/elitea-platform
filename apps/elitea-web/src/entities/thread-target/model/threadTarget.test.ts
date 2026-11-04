import { describe, expect, it } from 'vitest';

import { getGetPipelineScheduleResponseMock } from '@/shared/api/generated/applications/applications.msw';
import { EliteaApiError } from '@/shared/api/generated/mutator';

import { isThreadTarget, threadTargetBody, threadTargetDraftOf, threadTargetRefusal } from './threadTarget';

/** A refusal as `eliteaFetch` rejects it. */
function refused(status: number, body: unknown): EliteaApiError {
  return new EliteaApiError({ kind: 'http', status, url: '/api/v2/pipeline_schedules/prompt_lib/1/7', body });
}

describe('threadTarget', () => {
  it('reads a stored workspace_thread schedule (the v2.yaml example) as a draft with its grant', () => {
    const stored = getGetPipelineScheduleResponseMock();
    expect(isThreadTarget(stored)).toBe(true);
    expect(threadTargetDraftOf(stored)).toEqual({
      conversationUuid: '6f1c2d3e-4a5b-4c6d-8e7f-901234567890',
      prompt: 'Run the nightly checks and fix what fails.',
      grant: { kind: 'set', grant: { continue_step_limit: true, continue_output_limit: true, goal_check: true, chain: { max_continuations: 5 } } },
    });
  });

  it('keeps only the parts of a stored grant that these origins take', () => {
    const draft = threadTargetDraftOf({
      target_kind: 'workspace_thread',
      conversation_uuid: 'c-1',
      prompt: 'go',
      grant: { continue_step_limit: true, goal_check: false, sensitive_tools: [{ toolkit_name: 'github', tool_name: 'merge' }], grant_id: '0b6f3c1e-0000-4000-8000-000000000000' },
    });
    expect(draft.grant).toEqual({ kind: 'set', grant: { continue_step_limit: true, goal_check: false } });
  });

  it('reads a pipeline schedule as an empty draft', () => {
    expect(isThreadTarget({ target_kind: 'pipeline' })).toBe(false);
    expect(threadTargetDraftOf({ target_kind: 'pipeline' })).toEqual({ conversationUuid: '', prompt: '', grant: { kind: 'default' } });
  });

  it('sends no grant when the person did not change it', () => {
    expect(threadTargetBody({ conversationUuid: 'c-1', prompt: 'Run the checks.', grant: { kind: 'default' } })).toEqual({
      ok: true,
      body: { target_kind: 'workspace_thread', conversation_uuid: 'c-1', prompt: 'Run the checks.' },
    });
  });

  it('marks a missing thread, an empty or too long message, and chain limits that are not valid', () => {
    const result = threadTargetBody({ conversationUuid: '', prompt: '   ', grant: { kind: 'invalid' } });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.thread).toBe('Select a thread.');
    expect(result.errors.prompt).toBe('Type the message that each run starts with.');
    expect(result.errors.grantInvalid).toBe(true);
    const long = threadTargetBody({ conversationUuid: 'c-1', prompt: 'x'.repeat(32769), grant: { kind: 'default' } });
    expect(long.ok ? undefined : long.errors.prompt).toBe('The message can have at most 32768 characters.');
  });

  it('puts each refusal of the save on its field', () => {
    expect(threadTargetRefusal(refused(422, { error: 'this conversation was never handed off to the cloud; hand it off from the desktop first', code: 'not_handed_off' }))?.thread).toBe(
      'This thread was never sent to the cloud. Send it to the cloud from the desktop app first.',
    );
    expect(threadTargetRefusal(refused(422, { error: 'governed', code: 'thread_not_governable' }))?.thread).toContain('a governed schedule or webhook needs a thread whose agent is this version');
    expect(threadTargetRefusal(refused(404, { error: 'no such conversation' }))?.thread).toBe('This thread is not in this project, or you are not a participant of it.');
    const grantBody = { error: 'unattended_grant_refused', reason: 'origin_not_allowed', field: 'sensitive_tools', message: 'The unattended grant was refused: origin_not_allowed.' };
    expect(threadTargetRefusal(refused(422, grantBody))).toEqual({ grant: grantBody });
    expect(threadTargetRefusal(refused(409, { error: 'busy', code: 'thread_busy' }))?.form).toBe('The thread is busy in the cloud. Try again when the current turn ends.');
    expect(threadTargetRefusal(refused(429, { error: 'too many' }))?.form).toBe('This thread started too many turns. Wait a few minutes and try again.');
    expect(threadTargetRefusal(refused(400, { error: '`target_kind` of a schedule must be `pipeline` or `workspace_thread`' }))?.form).toBe(
      '`target_kind` of a schedule must be `pipeline` or `workspace_thread`',
    );
  });

  it('is not a refusal of the target for a server error or no error', () => {
    expect(threadTargetRefusal(refused(500, { error: 'boom' }))).toBeUndefined();
    expect(threadTargetRefusal(undefined)).toBeUndefined();
    expect(threadTargetRefusal(new Error('network'))).toBeUndefined();
  });
});
