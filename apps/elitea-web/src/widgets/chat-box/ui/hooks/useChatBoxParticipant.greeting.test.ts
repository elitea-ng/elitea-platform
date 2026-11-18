import { describe, expect, it } from 'vitest';

import { readAgentGreeting } from './useChatBoxParticipant';

// #1987 / #1988: the agent's saved welcome message and starters never reached
// the chat, because nothing read them off the participant's version details.
describe('readAgentGreeting', () => {
  it('reads the welcome message and the non-blank starters off the bound version', () => {
    expect(
      readAgentGreeting({
        version_details: {
          welcome_message: '  Welcome! I am your test assistant.  ',
          conversation_starters: ['How do I create a new agent?', '', 'What toolkits are available?', 7],
        },
      }),
    ).toEqual({
      welcomeMessage: 'Welcome! I am your test assistant.',
      starters: ['How do I create a new agent?', 'What toolkits are available?'],
    });
  });

  it('answers nothing for a blank welcome message or a missing version', () => {
    expect(readAgentGreeting({ version_details: { welcome_message: '   ' } })).toEqual({
      welcomeMessage: undefined,
      starters: undefined,
    });
    expect(readAgentGreeting(undefined)).toEqual({ welcomeMessage: undefined, starters: undefined });
    expect(readAgentGreeting({})).toEqual({ welcomeMessage: undefined, starters: undefined });
  });
});
