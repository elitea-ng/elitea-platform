/**
 * The main chat page's "Allow attachments" gate (issue #905, ELITEA-0509) —
 * the pure predicate behind `useChatBoxAttachmentsGate`.
 */
import { describe, expect, it } from 'vitest';

import type { Participant } from '@/entities/participant';

import { shouldDisableParticipantAttachments } from './useChatBoxAttachmentsGate';

describe('shouldDisableParticipantAttachments (issue 905)', () => {
  const agent = (): Participant => ({ id: '7', entityName: 'application' });
  const pipeline = (): Participant => ({ id: '8', entityName: 'pipeline' });

  it('disables attachments for an agent whose internal_tools has no "attachments" entry', () => {
    expect(shouldDisableParticipantAttachments(agent(), [])).toBe(true);
    expect(shouldDisableParticipantAttachments(agent(), ['internal_mcp'])).toBe(true);
  });

  it('enables attachments for an agent whose "Allow attachments" toggle is on', () => {
    expect(shouldDisableParticipantAttachments(agent(), ['attachments'])).toBe(false);
    expect(shouldDisableParticipantAttachments(pipeline(), ['internal_mcp', 'attachments'])).toBe(false);
  });

  it('treats unresolved internal tools as off, the same way the agent editor does', () => {
    expect(shouldDisableParticipantAttachments(agent(), undefined)).toBe(true);
    expect(shouldDisableParticipantAttachments(pipeline(), undefined)).toBe(true);
  });

  it('never gates a plain-model conversation or a non-agent participant', () => {
    expect(shouldDisableParticipantAttachments(undefined, undefined)).toBe(false);
    expect(shouldDisableParticipantAttachments({ id: '9', entityName: 'datasource' } as unknown as Participant, [])).toBe(false);
  });
});
