/**
 * The "Credentials / Personal data" preset of Admin › LLM Governance
 * (ADR-0043 decision 1, issue 2012).
 *
 * One screen picks what to check (credentials, personal data), what to do with
 * a match (block, mask or warn) and where (the request, the answer). The
 * preset builds the guardrail entries that say so and sends each through the
 * ordinary create route. It adds no endpoint and no storage: an entry made
 * here is edited and deleted like any other.
 *
 * A guardrail entry has ONE stage, so a preset that checks both stages makes
 * two entries. The detectors are the gateway's built-in rule packs: `secrets`
 * (the platform credential rules) and `personal_data` (e-mail, phone, payment
 * card with a Luhn check, IBAN with mod-97).
 */
import type { GovernanceWrite } from './api/adminGovernanceApi';

export const PRESET_ACTIONS = ['block', 'mask', 'warn'] as const;
export type PresetAction = (typeof PRESET_ACTIONS)[number];

export interface GuardrailPresetChoice {
  readonly name: string;
  readonly credentials: boolean;
  readonly credentialsAction: PresetAction;
  readonly personalData: boolean;
  readonly personalDataAction: PresetAction;
  readonly input: boolean;
  readonly output: boolean;
}

export const DEFAULT_PRESET: GuardrailPresetChoice = {
  name: 'Credentials and personal data',
  credentials: true,
  credentialsAction: 'block',
  personalData: true,
  personalDataAction: 'mask',
  input: true,
  output: true,
};

/** Whether the choice can make an entry: a name, one pack and one stage. */
export function presetIsComplete(choice: GuardrailPresetChoice): boolean {
  return choice.name.trim() !== '' && (choice.credentials || choice.personalData) && (choice.input || choice.output);
}

function detectorsOf(choice: GuardrailPresetChoice): Record<string, unknown>[] {
  const detectors: Record<string, unknown>[] = [];
  if (choice.credentials) detectors.push({ name: 'credentials', type: 'secrets', action: choice.credentialsAction });
  if (choice.personalData) detectors.push({ name: 'personal-data', type: 'personal_data', action: choice.personalDataAction });
  return detectors;
}

/**
 * The entries a choice makes, input first. Platform-wide: no scope, so the
 * entry covers every project until an administrator narrows it by editing it.
 */
export function presetEntries(choice: GuardrailPresetChoice): readonly GovernanceWrite[] {
  if (!presetIsComplete(choice)) return [];
  const base = choice.name.trim();
  const stages = [choice.input ? 'input' : undefined, choice.output ? 'output' : undefined];
  return stages.flatMap((stage) =>
    stage === undefined
      ? []
      : [{
          type: 'guardrail',
          name: `${base} - ${stage}`,
          enabled: true,
          data: { guardrail: { stage, detectors: detectorsOf(choice) } },
        }],
  );
}
