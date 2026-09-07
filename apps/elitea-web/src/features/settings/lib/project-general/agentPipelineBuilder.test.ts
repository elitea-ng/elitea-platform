import { describe, expect, it } from 'vitest';

import { buildInternalMcpUpdate, selectInternalMcpEnabled } from './agentPipelineBuilder';

describe('selectInternalMcpEnabled', () => {
  it('reads the stored flag', () => {
    expect(selectInternalMcpEnabled({ personalization: { default_internal_mcp_enabled: false } })).toBe(false);
    expect(selectInternalMcpEnabled({ personalization: { default_internal_mcp_enabled: true } })).toBe(true);
  });

  it('defaults to ON when the key was never written', () => {
    // A live deployment shows the switch ON for an untouched account. An OFF
    // default would draw a switch that disagrees with the running feature.
    expect(selectInternalMcpEnabled({ personalization: {} })).toBe(true);
    expect(selectInternalMcpEnabled({})).toBe(true);
    expect(selectInternalMcpEnabled(undefined)).toBe(true);
  });

  it('ignores a non-object or non-boolean value rather than coercing it', () => {
    expect(selectInternalMcpEnabled({ personalization: 'nope' })).toBe(true);
    expect(selectInternalMcpEnabled({ personalization: [] })).toBe(true);
    expect(selectInternalMcpEnabled({ personalization: { default_internal_mcp_enabled: 0 } })).toBe(true);
  });
});

describe('buildInternalMcpUpdate', () => {
  it('carries every other personalization key forward', () => {
    // The PUT replaces the whole blob. Dropping a key here deletes the user's
    // persona and default instructions as a side effect of a switch flip.
    const author = {
      name: 'Ann',
      description: 'desc',
      avatar: 'a.png',
      personalization: {
        default_internal_mcp_enabled: true,
        persona: 'concise',
        default_user_instructions: 'be brief',
      },
    };

    expect(buildInternalMcpUpdate(author, false)).toEqual({
      name: 'Ann',
      description: 'desc',
      avatar: 'a.png',
      personalization: {
        default_internal_mcp_enabled: false,
        persona: 'concise',
        default_user_instructions: 'be brief',
      },
    });
  });

  it('omits top-level fields the profile did not carry, instead of sending undefined', () => {
    expect(buildInternalMcpUpdate({ personalization: {} }, true)).toEqual({
      personalization: { default_internal_mcp_enabled: true },
    });
  });

  it('writes the flag even when the profile has no personalization at all', () => {
    expect(buildInternalMcpUpdate(undefined, false)).toEqual({
      personalization: { default_internal_mcp_enabled: false },
    });
  });
});
