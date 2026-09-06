/**
 * DefaultSettingsSelects — a Settings › AI Providers section's default-model
 * pickers.
 *
 * Split out of `ConfigurationSection.tsx` only because that file passed the
 * 400-line budget once the accordion landed; it is used by nothing else.
 */
import Box from '@mui/material/Box';

import { SingleSelect } from '@/shared/ui/SingleSelect';

import type { AdditionalDefaultSetting } from './configurationSection.types';
import type { ConfigurationSectionStyles } from './configurationSection.styles';

/**
 * The section's default-model pickers, laid out as production lays them out:
 * a ROW of bordered pills, each holding its own label and its select.
 *
 * They used to be a stacked column of bare `SingleSelect`s whose `label` prop
 * was dropped on the floor — `defaultSettingLabel` is a ReactNode (a label
 * plus an info icon), and the code passed it through
 * `typeof label === 'string' ? label : ''`, so all three selects rendered
 * with NO label at all. Three identical full-width rows reading "None" was
 * the whole of Settings › AI Providers' default-model UI.
 */
export function DefaultSettingsSelects({
  canEdit, defaultSettingValue, defaultSettingLabel, defaultSettingOptions,
  onChangeDefaultSetting, defaultSettingError, additionalDefaultSettings, styles,
}: {
  canEdit: boolean;
  defaultSettingValue: string;
  defaultSettingLabel?: React.ReactNode;
  defaultSettingOptions?: Array<{ value: string; label: string }>;
  onChangeDefaultSetting?: (value: string) => void;
  defaultSettingError?: string;
  additionalDefaultSettings?: AdditionalDefaultSetting[];
  styles: ConfigurationSectionStyles;
}) {
  return (
    <Box sx={styles.defaultSettingsContainer}>
      <Box sx={styles.defaultSettingPill}>
        {defaultSettingLabel}
        <SingleSelect
          value={defaultSettingValue}
          onChange={onChangeDefaultSetting || (() => {})}
          options={defaultSettingOptions ?? []}
          disabled={!canEdit}
          error={defaultSettingError ?? ''}
          sx={styles.defaultSettingSelect}
        />
      </Box>
      {additionalDefaultSettings
        ?.filter((s): s is NonNullable<typeof s> => Boolean(s))
        .map((setting) => (
          <Box key={setting.key ?? 'additional'} sx={styles.defaultSettingPill}>
            {setting.label}
            <SingleSelect
              value={setting.value ?? ''}
              onChange={setting.onChange ?? (() => {})}
              options={setting.options ?? []}
              disabled={!canEdit}
              error={setting.error ?? ''}
              sx={styles.defaultSettingSelect}
            />
          </Box>
        ))}
    </Box>
  );
}

