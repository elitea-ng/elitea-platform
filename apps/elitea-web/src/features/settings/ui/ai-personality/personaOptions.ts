/**
 * Persona catalogue for Settings › AI Personality.
 *
 * Baseline: `EliteaUI/src/common/constants.js:1107-1132` (`PERSONA_OPTIONS`,
 * `DEFAULT_PERSONA`, `PERSONA_INSTRUCTIONS_PLACEHOLDERS`). The label,
 * description and per-persona placeholder are all user-visible, so each one
 * goes through `t()` (R-T3) instead of shipping the baseline's raw English.
 *
 * `t()` is called from the accessor functions, not at module scope: a
 * module-level constant would be frozen at import time, before
 * `i18n.changeLanguage()` ever runs.
 */
import { t } from '@/shared/i18n';
import type { SingleSelectOption } from '@/shared/ui/SingleSelectMenuItem';

/** Persona ids, in the baseline's display order. */
const PERSONA_VALUES = ['generic', 'qa', 'nerdy', 'quirky', 'cynical', 'none', 'bare'] as const;

export type PersonaValue = (typeof PERSONA_VALUES)[number];

/** `constants.js:1121`. */
export const DEFAULT_PERSONA: PersonaValue = 'generic';

/**
 * The one persona with no personality overlay — and therefore no user
 * instructions to author (baseline `AIPersonalityPersonalization.jsx:14`).
 */
export const NONE_PERSONA: PersonaValue = 'none';

/** Every persona starts with an empty instructions slot (`profile.helpers.js:11-16`). */
export function emptyPersonalityInstructions(): Record<string, string> {
  return Object.fromEntries(PERSONA_VALUES.map((value) => [value, '']));
}

/** Label + description pairs — rendered by `SingleSelectMenuItem`'s two-line option row. */
export function buildPersonaOptions(): SingleSelectOption[] {
  return [
    {
      value: 'generic',
      label: t('settings.aiPersonality.persona.generic.label', 'Generic'),
      description: t('settings.aiPersonality.persona.generic.description', 'Balanced, professional assistant'),
    },
    {
      value: 'qa',
      label: t('settings.aiPersonality.persona.qa.label', 'QA'),
      description: t('settings.aiPersonality.persona.qa.description', 'Precise, technical, testing-focused'),
    },
    {
      value: 'nerdy',
      label: t('settings.aiPersonality.persona.nerdy.label', 'Nerdy'),
      description: t('settings.aiPersonality.persona.nerdy.description', 'Technical deep-dives, detailed explanations'),
    },
    {
      value: 'quirky',
      label: t('settings.aiPersonality.persona.quirky.label', 'Quirky'),
      description: t('settings.aiPersonality.persona.quirky.description', 'Creative, playful, thinking outside the box'),
    },
    {
      value: 'cynical',
      label: t('settings.aiPersonality.persona.cynical.label', 'Cynical'),
      description: t('settings.aiPersonality.persona.cynical.description', 'Skeptical, challenges assumptions'),
    },
    {
      value: 'none',
      label: t('settings.aiPersonality.persona.none.label', 'None'),
      description: t('settings.aiPersonality.persona.none.description', 'No personality overlay applied'),
    },
    {
      value: 'bare',
      label: t('settings.aiPersonality.persona.bare.label', 'Bare'),
      description: t(
        'settings.aiPersonality.persona.bare.description',
        'No {{product}} identity — only your instructions plus tool-required guidance',
      ),
    },
  ];
}

/**
 * Contextual placeholder for the instructions field (`constants.js:1124-1132`).
 * The five named personas each get their own sentence; anything else falls
 * back to the generic wording the baseline uses for `none`/`bare`.
 */
export function personaInstructionsPlaceholder(persona: string): string {
  const named: Record<string, string> = {
    generic: t(
      'settings.aiPersonality.instructionsPlaceholder.generic',
      'No custom instructions for the Generic persona yet. Type here to add some.',
    ),
    qa: t(
      'settings.aiPersonality.instructionsPlaceholder.qa',
      'No custom instructions for the QA persona yet. Type here to add some.',
    ),
    nerdy: t(
      'settings.aiPersonality.instructionsPlaceholder.nerdy',
      'No custom instructions for the Nerdy persona yet. Type here to add some.',
    ),
    quirky: t(
      'settings.aiPersonality.instructionsPlaceholder.quirky',
      'No custom instructions for the Quirky persona yet. Type here to add some.',
    ),
    cynical: t(
      'settings.aiPersonality.instructionsPlaceholder.cynical',
      'No custom instructions for the Cynical persona yet. Type here to add some.',
    ),
  };
  return (
    named[persona] ??
    t(
      'settings.aiPersonality.instructionsPlaceholder.fallback',
      'No custom instructions for this persona yet. Type here to add some.',
    )
  );
}
