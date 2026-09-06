// @ts-nocheck
/**
 * ConfigurationSection — displays a group of configurations with optional default-setting select.
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/ai-configuration/Configuration/ConfigurationSection.jsx`.
 */
import { memo, useMemo } from 'react';
import { useTheme } from '@mui/material/styles';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';
import { PERMISSIONS } from '@/shared/lib/permissions';
/*
 * [#71] These two replace this file's own former `getGroupLabel`/`sortByName`,
 * which were ad-hoc reimplementations of them. The baseline calls exactly these
 * two helpers from its own ConfigurationSection.jsx (`ConfigurationHelpers
 * .getConfigurationGroup` at :40, `.sortConfigurationsByDisplayName` at :50 and
 * :61), so using them here is both the dedup and the closer parity. Grouping is
 * behaviour-identical (same third-party keyword list, same OpenAI/Anthropic
 * type lists, same fallback); sorting now goes through
 * `getConfigurationDisplayName`, whose longer name-priority chain is ported
 * verbatim from the baseline's configuration.helpers.js:40-52 and is a superset
 * of the four keys the local copy looked at.
 */
import {
  getConfigurationGroup,
  sortConfigurationsByDisplayName,
} from '@/features/settings/lib/ai-configuration/configuration.helpers';

import { AIProviderAccordion, type AIProviderAccordionMetaItem } from './AIProviderAccordion';
import { ConfigCards } from './ConfigCards';
import { getStyles } from './configurationSection.styles';
import { DefaultSettingsSelects } from './DefaultSettingsSelects';
import type { AdditionalDefaultSetting } from './configurationSection.types';

const GROUP_ORDER = ['OpenAI', 'Anthropic', 'Other LLM Providers'];

/** `label` of the option whose `value` matches — the collapsed summary shows
 * the model's NAME, never the `name<<>>project_id` key the select carries. */
function optionLabelOf(options: Array<{ value: string; label: string }> | undefined, value: string): string {
  return options?.find((option) => option.value === value)?.label ?? '';
}

export type { AdditionalDefaultSetting } from './configurationSection.types';

interface ConfigurationSectionProps {
  title: string;
  configurations: readonly Record<string, unknown>[];
  /** Currently-selected project id — gates edit permission
   * (`PERMISSIONS.configuration.update`) and drives click-to-edit
   * navigation, same as the old app's `useSelectedProjectId()` read. */
  projectId: string;
  isLoading?: boolean;
  hasDefaultSetting?: boolean;
  defaultSettingLabel?: React.ReactNode;
  defaultSettingValue?: string;
  defaultSettingOptions?: Array<{ value: string; label: string }>;
  onChangeDefaultSetting?: (value: string) => void;
  /** Message from the last failed save of this section's default model.
   * Without it a failed save shows nothing: the select is controlled from
   * query data, so it silently returns to the previous default. */
  defaultSettingError?: string | undefined;
  additionalDefaultSettings?: AdditionalDefaultSetting[];
  /**
   * How the section PRESENTS its cards, grouped as one prop rather than
   * three: `ConfigurationSection` sits exactly on the §3.5 12-prop budget,
   * and the accordion needed two more (`defaultExpanded`, `testId`).
   */
  display?: {
    /** Splits the cards under per-provider headings (LLMs only). */
    groupByProvider?: boolean;
    /** Only the first section (LLMs) opens on arrival, as in production. */
    defaultExpanded?: boolean;
    testId?: string;
  };
}

/**
 * Groups configurations by provider label and sorts each group.
 */
function groupConfigurationsByProvider(
  configurations: Record<string, unknown>[],
  sortFn: (a: Record<string, unknown>, b: Record<string, unknown>) => number,
): Record<string, Record<string, unknown>[]> | null {
  const groups: Record<string, Record<string, unknown>[]> = {};
  for (const config of configurations) {
    const groupLabel = getConfigurationGroup(
      config.name as string | undefined,
      config.type as string | undefined,
      config.label as string | undefined,
    );
    if (!groups[groupLabel]) groups[groupLabel] = [];
    groups[groupLabel].push(config);
  }
  for (const groupLabel of Object.keys(groups)) {
    groups[groupLabel].sort(sortFn);
  }
  return groups;
}

/**
 * Local per-user edit-permission check — mirrors the old app's
 * `useCheckPermission().checkPermission(PERMISSIONS.configuration.update)`
 * (`ConfigurationSection.jsx:64-65`), reimplemented against this app's
 * `usePermissionList(projectId, ...)` query the same way
 * `features/pipelines/lib/useHasPermission.ts` / `features/agents/lib/
 * useHasPermission.ts` / `features/chat-conversation-list/lib/
 * useHasPermission.ts` already do for their own features. Kept local
 * (not imported from a sibling feature) — `no-sideways-features` forbids
 * `features/settings` reaching into another feature's internals.
 */
function useCanEditConfiguration(projectId: string): boolean {
  const query = usePermissionList(projectId, { query: { enabled: !!projectId } });

  const permissions = useMemo(() => {
    const list = query.data?.data as Permission[] | undefined;
    if (!list) return new Set<string>();
    return new Set(list.filter((entry) => entry.enabled).map((entry) => entry.name));
  }, [query.data]);

  return permissions.has(PERMISSIONS.configuration.update);
}

export default memo(function ConfigurationSection({
  title,
  configurations,
  projectId,
  isLoading,
  hasDefaultSetting,
  defaultSettingLabel,
  defaultSettingValue = '',
  defaultSettingOptions = [],
  onChangeDefaultSetting,
  defaultSettingError,
  additionalDefaultSettings = [],
  display = {},
}: ConfigurationSectionProps) {
  const { groupByProvider: groupTheModelsByProvider = false, defaultExpanded = false, testId: sectionTestId } = display;
  const theme = useTheme();
  const styles = getStyles(theme);
  const canEdit = useCanEditConfiguration(projectId);

  const groupedConfigurations = useMemo(() => {
    if (!groupTheModelsByProvider || !configurations?.length) return null;
    return groupConfigurationsByProvider(configurations, sortConfigurationsByDisplayName);
  }, [configurations, groupTheModelsByProvider]);

  const sortedConfigurations = useMemo(() => {
    if (groupTheModelsByProvider) return configurations;
    return [...(configurations || [])].sort(sortConfigurationsByDisplayName);
  }, [configurations, groupTheModelsByProvider]);

  if (isLoading) {
    return (
      <Box sx={styles.container}>
        <Typography variant="headingSmall" sx={styles.title}>{title}</Typography>
        <Typography variant="bodyMedium">{t('ai-configuration.section.loading', 'Loading...')}</Typography>
      </Box>
    );
  }

  if (!configurations || configurations.length === 0) return null;

  return (
    <ConfigurationSectionBody
      title={title}
      styles={styles}
      projectId={projectId}
      canEdit={canEdit}
      count={configurations.length}
      defaultExpanded={defaultExpanded}
      {...(sectionTestId === undefined ? {} : { sectionTestId })}
      defaultSetting={{
        has: hasDefaultSetting,
        value: defaultSettingValue,
        label: defaultSettingLabel,
        options: defaultSettingOptions,
        onChange: onChangeDefaultSetting,
        error: defaultSettingError,
        additional: additionalDefaultSettings,
      }}
      grouping={{
        byProvider: groupTheModelsByProvider,
        grouped: groupedConfigurations,
        sorted: sortedConfigurations,
      }}
    />
  );
});

interface ConfigurationSectionDefaultSetting {
  has?: boolean;
  value: string;
  label?: React.ReactNode;
  options?: Array<{ value: string; label: string }>;
  onChange?: (value: string) => void;
  error?: string;
  additional?: AdditionalDefaultSetting[];
}

interface ConfigurationSectionGrouping {
  byProvider?: boolean;
  grouped?: Record<string, Record<string, unknown>[]> | null;
  sorted?: readonly Record<string, unknown>[];
}

function ConfigurationSectionBody({
  title, styles, projectId, canEdit, count, defaultExpanded, sectionTestId, defaultSetting, grouping,
}: {
  title: string;
  styles: ReturnType<typeof getStyles>;
  projectId: string;
  canEdit: boolean;
  count: number;
  defaultExpanded: boolean;
  sectionTestId?: string;
  defaultSetting: ConfigurationSectionDefaultSetting;
  grouping: ConfigurationSectionGrouping;
}) {
  /* The collapsed summary answers "what is this section set to?" without
     opening it — the production page's `metaItems`. */
  const metaItems: AIProviderAccordionMetaItem[] = defaultSetting.has
    ? [
        {
          label: defaultSetting.label,
          value: optionLabelOf(defaultSetting.options, defaultSetting.value),
        },
        ...(defaultSetting.additional ?? []).map((setting) => ({
          label: setting.label,
          value: optionLabelOf(setting.options, setting.value),
        })),
      ]
    : [];

  return (
    <Box sx={styles.container}>
      <AIProviderAccordion
        title={title}
        count={count}
        metaItems={metaItems}
        defaultExpanded={defaultExpanded}
        {...(sectionTestId === undefined ? {} : { 'data-testid': sectionTestId })}
      >
        {defaultSetting.has && (
          <DefaultSettingsSelects
            canEdit={canEdit}
            defaultSettingValue={defaultSetting.value}
            defaultSettingLabel={defaultSetting.label}
            defaultSettingOptions={defaultSetting.options}
            onChangeDefaultSetting={defaultSetting.onChange}
            defaultSettingError={defaultSetting.error}
            additionalDefaultSettings={defaultSetting.additional}
            styles={styles}
          />
        )}

        {grouping.byProvider && grouping.grouped ? (
          GROUP_ORDER.map((groupLabel, index) => {
            const groupConfigs = grouping.grouped?.[groupLabel] ?? [];
            if (groupConfigs.length === 0) return null;
            return (
              <Box key={groupLabel} sx={styles.groupContainer(index < GROUP_ORDER.length - 1)}>
                <Typography variant="subtitle" color="text.primary" sx={styles.groupLabel}>
                  {groupLabel}
                </Typography>
                <ConfigCards
                  configurations={groupConfigs}
                  projectId={projectId}
                  canEdit={canEdit}
                  defaultSettingValue={defaultSetting.value}
                  styles={styles}
                />
              </Box>
            );
          })
        ) : (
          <ConfigCards
            configurations={grouping.sorted ?? []}
            projectId={projectId}
            canEdit={canEdit}
            defaultSettingValue={defaultSetting.value}
            styles={styles}
          />
        )}
      </AIProviderAccordion>
    </Box>
  );
}
