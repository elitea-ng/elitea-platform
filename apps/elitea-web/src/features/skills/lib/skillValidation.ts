import { exceedsLimit } from '@/shared/lib/textLimits';

import type { SkillDraft, SkillWriteInput } from '../model/types';

export const SKILL_NAME_MAX_LENGTH = 64;
export const SKILL_DESCRIPTION_MAX_LENGTH = 512;
export const SKILL_INSTRUCTIONS_MAX_LENGTH = 50_000;

export interface SkillValidationErrors {
  readonly name?: string;
  readonly description?: string;
  readonly instructions?: string;
}

export function validateSkill(input: SkillWriteInput | SkillDraft): SkillValidationErrors {
  const errors: { name?: string; description?: string; instructions?: string } = {};
  const name = input.name.trim();
  const description = input.description.trim();
  const instructions = input.instructions.trim();

  if (name === '') errors.name = 'Name is required.';
  else if (exceedsLimit(name, SKILL_NAME_MAX_LENGTH)) errors.name = `Name must be ${SKILL_NAME_MAX_LENGTH} characters or fewer.`;

  if (description === '') errors.description = 'Description is required.';
  else if (exceedsLimit(description, SKILL_DESCRIPTION_MAX_LENGTH)) {
    errors.description = `Description must be ${SKILL_DESCRIPTION_MAX_LENGTH} characters or fewer.`;
  }

  if (instructions === '') errors.instructions = 'Instructions are required.';
  else if (instructions.length > SKILL_INSTRUCTIONS_MAX_LENGTH) {
    errors.instructions = `Instructions must be ${SKILL_INSTRUCTIONS_MAX_LENGTH} characters or fewer.`;
  }

  return errors;
}

export function isSkillValid(input: SkillWriteInput | SkillDraft): boolean {
  return Object.keys(validateSkill(input)).length === 0;
}
