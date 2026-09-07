import { t } from '@/shared/i18n';

/**
 * The three Publishing Terms blocks, transcribed from the production dialog
 * (read-only capture of `next.elitea.ai` on 2026-09-06).
 *
 * They are kept verbatim because two of the three are statements about what
 * the SERVER does, and this app's server does the same thing:
 *
 *  - the exclusions notice matches the catalogue twin, which deliberately
 *    carries no tool or skill attachment rows
 *    (`internal/api/v2/eliteacore/catalog_mirror.go`);
 *  - the best-practice list matches `runPublishValidation`'s own rules
 *    (generic name, missing description, missing tag, missing instructions);
 *  - the administrative-rights block matches the publish guardrail, which an
 *    operator can flip deployment-wide, and Unpublish, which the author owns.
 *
 * Rewriting them would make the dialog promise something different from what
 * the code does, which is the failure mode this text exists to prevent.
 */
export interface PublishingTerm {
  readonly heading: string;
  readonly lines: readonly string[];
}

/**
 * A function, not a module-scope constant: `t()` reads the live i18n bundle,
 * and a constant evaluated at import time would freeze the copy at whatever
 * language was loaded when this module was first pulled in.
 */
export function getPublishingTerms(): readonly PublishingTerm[] {
  return [
  {
    heading: t('features.agentLifecycle.terms.exclusionsHeading', '1 - Exclusions Notice'),
    lines: [
      t(
        'features.agentLifecycle.terms.exclusionsIntro',
        'When publishing, certain components are removed from the public version for security and privacy:',
      ),
      t('features.agentLifecycle.terms.exclusionsCredentials', 'Credentials and API keys'),
      t('features.agentLifecycle.terms.exclusionsDatasources', 'Private datasource connections'),
      t('features.agentLifecycle.terms.exclusionsEnvironment', 'Environment-specific configurations'),
      t('features.agentLifecycle.terms.exclusionsTools', 'Internal tool endpoints'),
      t(
        'features.agentLifecycle.terms.exclusionsConsumers',
        'Users of your published agent must supply their own credentials and configurations.',
      ),
    ],
  },
  {
    heading: t('features.agentLifecycle.terms.practicesHeading', '2 - Best Practices Requirements'),
    lines: [
      t('features.agentLifecycle.terms.practicesIntro', 'To give users a good experience, your agent should have:'),
      t('features.agentLifecycle.terms.practicesName', 'A clear, specific name'),
      t('features.agentLifecycle.terms.practicesDescription', 'A description that explains what it does'),
      t('features.agentLifecycle.terms.practicesTag', 'At least one tag, so users can find it'),
      t('features.agentLifecycle.terms.practicesInstructions', 'Full instructions for the model'),
      t('features.agentLifecycle.terms.practicesStarters', 'A welcome message and conversation starters'),
    ],
  },
  {
    heading: t('features.agentLifecycle.terms.rightsHeading', '3 - Administrative Rights'),
    lines: [
      t('features.agentLifecycle.terms.rightsIntro', 'When you publish, you agree that:'),
      t('features.agentLifecycle.terms.rightsAdmin', 'Platform administrators can unpublish your agent at any time'),
      t('features.agentLifecycle.terms.rightsAuthor', 'You keep the ability to unpublish your own agent'),
      t('features.agentLifecycle.terms.rightsVisibility', 'Published agents are visible to all users in the Catalog'),
      t('features.agentLifecycle.terms.rightsMetrics', 'Usage data can be collected for published agents'),
    ],
  },
  ];
}
