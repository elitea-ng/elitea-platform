/**
 * The words of a team thread viewed on mobile (ADR-0061 Parity, #3069): where each agent runs,
 * the "Ran outside the sandbox" marker and the live reply's activity label. The app has no
 * translation layer yet; every string of this feature lives here so one can replace it (as
 * `turnTakingCopy.ts` does for turn-taking).
 *
 * `activity` is the mobile catalogue of client-core's `ActivityLabels`: `activityLabel` fills
 * `{tool}` and `{name}`, so the wording matches the web and the desktop.
 */
import type { ActivityLabels } from '@elitea/client-core/conversation';

const ACTIVITY: ActivityLabels = {
  idle: 'Idle',
  thinking: 'Thinking',
  waking: 'Resuming',
  speaking: 'Writing',
  working: 'Working',
  running: 'Running {tool}',
  waitingForYou: 'Waiting for you',
  waitingForAgent: 'Waiting for {name}',
  anotherAgent: 'another agent',
  handedTo: 'Handed to {name}',
  done: 'Done',
  error: 'Error',
};

export const TEAM_COPY = {
  /** An agent that runs on the laptop with the folder. */
  executorLocal: 'Local',
  /** An agent that runs in the cloud with the conversation text only. */
  executorCloud: 'Cloud',
  executorLabel: (name: string, cloud: boolean) => (cloud ? `${name} runs in the cloud` : `${name} runs on the laptop`),
  ranOutsideSandbox: 'Ran outside the sandbox',
  ranOutsideSandboxLabel: 'This answer ran commands outside the sandbox, in a folder its owner trusts.',
  fullAuto: 'Ran in Full auto',
  fullAutoLabel: 'This answer ran in Full auto: the agent answered every approval for its owner, including those asked after reading outside content.',
  activity: ACTIVITY,
} as const;
