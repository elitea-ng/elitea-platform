/**
 * User-personalization persona options ported from
 * apps/elitea-ui/src/common/constants.js:1084-1098 (unit S3, spec §9.3).
 * `label`/`description` are user-visible copy (see S3 report re: S8).
 */
import { productName } from '@/shared/brand';
export const PERSONA_OPTIONS = [
  { label: 'Generic', value: 'generic', description: 'Balanced, professional assistant' },
  { label: 'QA', value: 'qa', description: 'Precise, technical, testing-focused' },
  { label: 'Nerdy', value: 'nerdy', description: 'Technical deep-dives, detailed explanations' },
  { label: 'Quirky', value: 'quirky', description: 'Creative, playful, thinking outside the box' },
  { label: 'Cynical', value: 'cynical', description: 'Skeptical, challenges assumptions' },
  { label: 'None', value: 'none', description: 'No personality overlay applied' },
  {
    label: 'Bare',
    value: 'bare',
    description: `No ${productName()} identity — only your instructions plus tool-required guidance`,
  },
] as const;

export const DEFAULT_PERSONA = 'generic';
