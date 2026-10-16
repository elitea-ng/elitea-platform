/**
 * The env part of the fake host's preview (`handoffIpc.fake.ts`): names and
 * value lengths only, as the host answers; nothing offered, with the notice,
 * when the login shell could not be read.
 */
import type { EnvVar, HandoffPolicy, PreviewEnv } from './handoffIpc';

export function previewEnv(envOffered: readonly EnvVar[], notice: string | undefined, policy: HandoffPolicy, selected: readonly string[]): PreviewEnv {
  const offered = notice === undefined ? envOffered.map((entry) => ({ ...entry })) : [];
  const present = offered.filter((entry) => selected.includes(entry.name));
  const bytes = present.reduce((sum, entry) => sum + entry.name.length + entry.length, 0);
  return {
    offered,
    selected: [...selected],
    missing: selected.filter((name) => !offered.some((entry) => entry.name === name)),
    vars: present.length,
    bytes,
    over_limit: present.length > policy.env_limits.max_vars || bytes > policy.env_limits.max_bytes,
    notice: notice ?? null,
  };
}
