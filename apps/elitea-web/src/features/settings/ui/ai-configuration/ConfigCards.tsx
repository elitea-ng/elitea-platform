/**
 * ConfigCards — the card grid inside one Settings › AI Providers section.
 *
 * Split out of `ConfigurationSection.tsx` only because that file passed the
 * 400-line budget once the accordion landed; it is used by nothing else.
 */
import Box from '@mui/material/Box';

import { useConfigurationNavigation } from '@/features/settings/lib/ai-configuration/useConfigurationNavigation';
import {
  toConfigurationId,
  useStoredConnectionHealthContext,
} from '@/features/settings/lib/ai-configuration/useStoredConnectionHealth';

import type { ConfigurationSectionStyles } from './configurationSection.styles';
import ConfigurationCard from './ConfigurationCard';

/**
 * Renders ConfigurationCards for the given array of configurations.
 * Extracted to keep ConfigurationSection below the complexity budget.
 */
export function ConfigCards({
  configurations,
  projectId,
  canEdit,
  defaultSettingValue,
  styles,
}: {
  configurations: readonly Record<string, unknown>[];
  projectId: string;
  canEdit: boolean;
  defaultSettingValue: string;
  styles: ConfigurationSectionStyles;
}) {
  // Old app: `ConfigurationCard.jsx`'s `handleCardClick` calls
  // `navigateToConfiguration(configuration.id, locationState)` on click,
  // routing into the configuration's edit view. Wired here (not passed
  // down as a prop) since the hook must be called from a component body.
  const { navigateToConfiguration } = useConfigurationNavigation();
  // Read here, not threaded from the panel: `ConfigurationSection` already
  // destructures 12 props, which IS the §3.5 component-props budget, and this
  // component never reads the value — it only forwards it. See
  // `StoredConnectionHealthView`'s own doc comment. The card boundary below
  // stays explicit props.
  const connectionHealth = useStoredConnectionHealthContext();

  return (
    <Box sx={styles.configurationsContainer}>
      {configurations.map((configuration, index) => {
        const cfg = configuration;
        const d = cfg.data as Record<string, unknown> | undefined;
        const configurationId = toConfigurationId(cfg.id);
        return (
          <ConfigurationCard
            key={`${(cfg.id as string) || (cfg.name as string)}-${index}`}
            configuration={configuration}
            projectId={projectId}
            canEdit={canEdit}
            isDefault={defaultSettingValue === `${(d?.name as string) ?? ''}<<>>${(cfg.project_id as string) ?? ''}`}
            onClick={navigateToConfiguration}
            health={connectionHealth.health[configurationId]}
            onRevalidate={connectionHealth.revalidate}
            isRevalidating={connectionHealth.revalidatingId === configurationId}
          />
        );
      })}
    </Box>
  );
}

