import type { ReactNode } from 'react';
import { useCallback, useId, useState } from 'react';

import type { SxProps, Theme } from '@mui/material/styles';
import ToggleButtonGroup from '@mui/material/ToggleButtonGroup';

import { t } from '@/shared/i18n';
import type { TabGroupButtonItem } from '../TabButtonItem';
import { TabButtonItem } from '../TabButtonItem';

export type { TabGroupButtonItem };

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface TabGroupButtonProps {
  items: TabGroupButtonItem[];
  /** Controlled selection. Omit and use `defaultValue` for uncontrolled use. */
  value?: string;
  defaultValue?: string;
  onChange?: (value: string) => void;
  size?: 'small' | 'medium' | 'large';
  disableTooltip?: boolean;
  ariaLabel?: string;
  id?: string;
  sx?: SxProps<Theme>;
  /**
   * Styles applied to EVERY button in the row — a fixed width, for instance.
   * A caller that needs one size for the whole group states it here. Reaching
   * from the group's own `sx` into the buttons underneath it names a MUI
   * internal class, which R-T6 (`elitea/no-mui-internal-selector`) bans
   * outside `shared/brand/mui-overrides/`.
   */
  itemSx?: SxProps<Theme>;
}

/**
 * An exclusive-choice row of `ToggleButton`s (e.g. switching between views),
 * built on MUI's `ToggleButtonGroup`. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/tab-group-button/TabGroupButton.jsx`.
 *
 * This is a `ToggleButtonGroup`/`ToggleButton` composition (button-group
 * semantics, `role="group"` of pressed buttons) — a genuinely different
 * family from `shared/ui/BaseTab`/`BaseTabs` (MUI `Tab`/`Tabs`, ARIA
 * `tablist`/`tab` semantics, styled via `MuiTab.ts`/`MuiTabs.ts`). Confirmed
 * by reading both baseline directories side by side
 * (`[fsd]/shared/ui/tab-group-button/` vs. `[fsd]/shared/ui/tabs/`) before
 * building this: they are not redundant with each other and one cannot be
 * aliased to the other.
 *
 * Deviations:
 *  - `ariaLabel` replaces the baseline's hardcoded, content-unrelated
 *    `aria-label="Toolkit View Toggler"` — a `shared/ui` component cannot
 *    hardcode a caller-specific label. Defaults to a generic translated
 *    fallback.
 *  - No `arrayBtn` default-example-data fallback (the baseline defaulted to
 *    a two-item "EliteA/Codemie" sample array when no `arrayBtn` prop was
 *    given, so a caller who forgot the prop would render fabricated
 *    content) — `items` is required.
 *  - No per-position joined-pill corner radius — see `TabButtonItem`'s doc
 *    comment for why (an already-flagged, out-of-scope token gap).
 */
export function TabGroupButton({
  items,
  value: controlledValue,
  defaultValue,
  onChange,
  size = 'small',
  disableTooltip,
  ariaLabel,
  id,
  sx,
  itemSx,
}: TabGroupButtonProps): ReactNode {
  const generatedId = useId();
  const groupId = id ?? `tab-group-button-${generatedId}`;
  const isControlled = controlledValue !== undefined;

  const [internalValue, setInternalValue] = useState<string | undefined>(() => defaultValue ?? items[0]?.value);

  const currentValue = isControlled ? controlledValue : internalValue;

  const handleChange = useCallback(
    (_event: React.MouseEvent<HTMLElement>, newValue: string | null) => {
      // MUI's `exclusive` mode passes `null` when the already-selected
      // button is clicked again — the baseline (and this port) keep exactly
      // one button always selected rather than allowing a fully-deselected
      // group.
      if (newValue === null) {
        return;
      }
      if (!isControlled) {
        setInternalValue(newValue);
      }
      onChange?.(newValue);
    },
    [isControlled, onChange],
  );

  return (
    <ToggleButtonGroup
      id={groupId}
      orientation="horizontal"
      size={size}
      value={currentValue ?? null}
      exclusive
      onChange={handleChange}
      aria-label={ariaLabel ?? t('shared.ui.tabGroupButton.ariaLabel', 'View toggle')}
      sx={sx}
    >
      {items.map((item) => (
        <TabButtonItem
          key={item.value}
          item={item}
          {...(disableTooltip !== undefined ? { disableTooltip } : {})}
          {...(itemSx !== undefined ? { sx: itemSx } : {})}
        />
      ))}
    </ToggleButtonGroup>
  );
}
