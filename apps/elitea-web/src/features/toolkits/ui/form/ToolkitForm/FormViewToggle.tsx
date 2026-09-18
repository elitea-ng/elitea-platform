import type { ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import type { SxProps, Theme } from '@mui/material/styles';

import { ToolkitViewOptions } from '@/shared/lib/enums';
import { FormIcon } from '@/shared/ui/icons/form-icon';
import { JsonIcon } from '@/shared/ui/icons/json-icon';
import { TabGroupButton } from '@/shared/ui/TabGroupButton';
import type { TabGroupButtonItem } from '@/shared/ui/TabGroupButton';

/**
 * Ported from `apps/elitea-ui/src/components/FormViewToggle.jsx` (44
 * lines) — the Form/Raw-Json toggle `ToolkitForm.tsx` shows above the
 * settings form.
 *
 * `TabGroupButton`'s `arrayBtn` prop -> `shared/ui`'s own `items`; its
 * `onChange={(_, newValue) => ...}` (MUI `ToggleButtonGroup` raw handler)
 * -> `shared/ui`'s already-adapted single-arg `onChange={(value) => ...}`
 * (that component's own port already did this adaptation — see its doc
 * comment). The whole-group `disabled` prop becomes a per-item `disabled`
 * (`shared/ui`'s `TabGroupButtonItem` shape has no group-level `disabled`).
 * `data-tour={SHARED_TOUR_TARGET_IDS.rawJsonTab}` (an interactive-tour
 * target id) is dropped — `features/interactive-tours` has no port in this
 * app and is out of this sub-unit's fence; the toggle itself is unaffected,
 * only the tour-highlight hook.
 */
export interface FormViewToggleProps {
  readonly view?: string;
  readonly onChangeView: (view: string) => void;
  readonly containerSX?: SxProps<Theme>;
  readonly disabled?: boolean;
}

export function FormViewToggle({ view = ToolkitViewOptions.Form, onChangeView, containerSX, disabled }: FormViewToggleProps): ReactNode {
  const items: TabGroupButtonItem[] = useMemo(
    () => [
      {
        value: ToolkitViewOptions.Form,
        label: 'Form',
        tooltip: 'Form view',
        icon: <FormIcon />,
        ...(disabled !== undefined ? { disabled } : {}),
      },
      {
        value: ToolkitViewOptions.Json,
        label: 'Raw Json',
        tooltip: 'Raw Json view',
        icon: <JsonIcon />,
        ...(disabled !== undefined ? { disabled } : {}),
      },
    ],
    [disabled],
  );

  const handleChange = useCallback(
    (newValue: string) => {
      if (newValue && newValue !== view) onChangeView(newValue);
    },
    [onChangeView, view],
  );

  return (
    <TabGroupButton
      items={items}
      value={view}
      onChange={handleChange}
      {...(containerSX !== undefined ? { sx: containerSX } : {})}
    />
  );
}
