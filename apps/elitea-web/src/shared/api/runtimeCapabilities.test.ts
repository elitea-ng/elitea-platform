import { describe, expect, it } from 'vitest';

import type { RuntimeCapabilities } from '@/shared/api/generated/model';

import { hiddenToolkitTypes, isInternalToolAvailable } from './runtimeCapabilities';

const RUST: RuntimeCapabilities = {
  worker: 'rust',
  internal_tools: {
    image_generation: false,
    data_analysis: true,
    internal_mcp: false,
    swarm: false,
    // Served natively by the Rust worker (planner, Smart Tools Selection and
    // attachments port), so not marked unavailable there.
    planner: true,
    lazy_tools_mode: true,
    attachments: true,
    subagents: true,
  },
  hidden_toolkit_types: ['jira', 'confluence', 'testio'],
};

describe('isInternalToolAvailable', () => {
  it('reports false for a tool the Rust worker still skips on a Rust deployment (#866)', () => {
    expect(isInternalToolAvailable(RUST, 'swarm')).toBe(false);
    expect(isInternalToolAvailable(RUST, 'image_generation')).toBe(false);
  });

  it('reports true on a Rust deployment for the tools it serves natively', () => {
    expect(isInternalToolAvailable(RUST, 'planner')).toBe(true);
    expect(isInternalToolAvailable(RUST, 'lazy_tools_mode')).toBe(true);
    expect(isInternalToolAvailable(RUST, 'attachments')).toBe(true);
    expect(isInternalToolAvailable(RUST, 'data_analysis')).toBe(true);
    expect(isInternalToolAvailable(RUST, 'subagents')).toBe(true);
  });

  // A name absent from internal_tools must not read as unavailable just
  // because it is absent.
  it('defaults to available for a tool name the capabilities response does not mention', () => {
    expect(isInternalToolAvailable(RUST, 'a_tool_this_endpoint_has_not_been_taught')).toBe(true);
  });

  it('defaults to available while the query has not resolved', () => {
    expect(isInternalToolAvailable(undefined, 'planner')).toBe(true);
  });
});

describe('hiddenToolkitTypes', () => {
  it('returns the configured worker set as-is (#865)', () => {
    const hidden = hiddenToolkitTypes(RUST);
    expect(hidden.has('jira')).toBe(true);
    expect(hidden.has('confluence')).toBe(true);
    expect(hidden.has('sql')).toBe(false);
  });

  it('is empty while the query has not resolved — never withholds a type on missing data', () => {
    expect(hiddenToolkitTypes(undefined).size).toBe(0);
  });
});
