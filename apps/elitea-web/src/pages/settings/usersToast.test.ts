/**
 * The toast lines, at the split the port lost: singular for one address,
 * plural for a batch, and "added" versus "invited by e-mail" for a deployment
 * with no SMTP (ADR-0024 WP7).
 */
import { describe, expect, it } from 'vitest';

import { errorMessage, invitePartialMessage, inviteSuccessMessage } from './usersToast';

function summary(over: Partial<Parameters<typeof inviteSuccessMessage>[0]> = {}) {
  return { invited: 1, alreadyMember: 0, invalid: 0, failed: 0, delivered: true, ...over };
}

describe('inviteSuccessMessage', () => {
  it('is singular for one address', () => {
    expect(inviteSuccessMessage(summary())).toBe('The user has been invited by e-mail');
  });

  it('is plural for a batch — the form this port did not have', () => {
    expect(inviteSuccessMessage(summary({ invited: 4 }))).toBe(
      'The users have been invited by e-mail',
    );
  });

  it('says "added" when no invitation e-mail went out, in both forms', () => {
    expect(inviteSuccessMessage(summary({ delivered: false }))).toContain('No invitation e-mail');
    expect(inviteSuccessMessage(summary({ invited: 3, delivered: false }))).toBe(
      'The users have been added. No invitation e-mail was sent.',
    );
  });
});

describe('invitePartialMessage', () => {
  it('counts the addresses that landed against the whole batch', () => {
    expect(invitePartialMessage(summary({ invited: 2, alreadyMember: 1, invalid: 1 }))).toBe(
      'Added 2 of 4. See the list below.',
    );
  });

  it('says nothing was added rather than "Added 0 of n"', () => {
    expect(invitePartialMessage(summary({ invited: 0, failed: 2 }))).toBe(
      'No user was added. See the list below.',
    );
  });
});

describe('errorMessage', () => {
  it('reads a real Error message', () => {
    expect(errorMessage(new Error('eliteaFetch: 403 from /x'))).toBe('eliteaFetch: 403 from /x');
  });

  it('stringifies anything else rather than rendering [object Object]', () => {
    expect(errorMessage('plain')).toBe('plain');
    expect(errorMessage(null)).toBe('null');
  });
});
