import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import KeyboardDoubleArrowLeftIcon from '@mui/icons-material/KeyboardDoubleArrowLeft';
import KeyboardDoubleArrowRightIcon from '@mui/icons-material/KeyboardDoubleArrowRight';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { GraphAdmissionGate } from './settings/GraphAdmissionGate';
import type { ViewMode } from '@/shared/lib/enums';

import { usePaneFold } from '../lib/hooks/useEditorPaneRoom';
import { useIsSmallWindow } from '../lib/hooks/useIsSmallWindow';

/**
 * Ported from `apps/elitea-ui/src/pages/Pipelines/Components/
 * GeneralFormPanel.jsx` — the collapsible left-hand panel wrapping the
 * pipeline's configuration form.
 *
 * **DISCLOSED REDESIGN — slot-based form content, matching the precedent
 * `features/pipelines/ui/PipelineEditor.tsx`'s own `renderConfigurationPanels`
 * already established for the SAME underlying baseline content:** the
 * baseline renders `<PipelineConfigurationForm applicationId viewMode />`
 * (`pages/Applications/Components/Applications/PipelineConfigurationForm.jsx`)
 * — the pipeline-specific assembly of `entities/application-form`'s
 * `ApplicationConfigurationLayout` fed six agent-domain-owned panels. No
 * `features/pipelines`-owned equivalent of that assembly exists anywhere in
 * this worktree (verified: `find src/features/pipelines -iname
 * '*ConfigurationForm*'` — zero hits) and it is not in this sub-unit's
 * (A2n) owned-file list. `renderConfigurationForm` makes that real
 * dependency explicit as a slot, matching `PipelineEditor.tsx`'s own
 * `renderConfigurationPanels` treatment of the identical gap for its own
 * (different, NewChat-embedded) composition of the same underlying form.
 *
 * **`useViewMode()` (baseline: a self-contained hook computing "Owner" vs.
 * "Public" from route/permission state) is dropped** — not in this
 * sub-unit's owned-file list, and no promoted equivalent exists. `viewMode`
 * becomes an explicit prop instead (defaulting to `ViewMode.Owner`),
 * matching this codebase's established "ambient hook -> explicit prop"
 * convention (e.g. `features/agents/ui/ConfigurationTab.tsx`'s own
 * `viewMode` prop for the identical situation).
 *
 * **Collapse icon:** the baseline's `DoubleLeftIcon`/`DoubleRightIcon`
 * (`components/Icons/*.jsx`, inline-JSX SVG components) were never ported
 * to `shared/ui/icons` (unit S2 ported `assets/*.svg` files only — verified:
 * no `double-left-icon`/`double-right-icon` exists there) and porting a new
 * shared icon is outside this sub-unit's scope. `@mui/icons-material`'s
 * `KeyboardDoubleArrowLeft`/`Right` are used instead — the same
 * `@mui/icons-material` escape hatch this codebase already uses elsewhere
 * for a real, not-yet-ported icon (`lib/flow-editor/helpers/node.helpers.
 * tsx`'s own `Repeat`/`RepeatOne` imports).
 */
export interface GeneralFormPanelProps {
  readonly applicationId: string | number | undefined;
  readonly onCollapsed: (collapsed: boolean) => void;
  /** Starts folded while the canvas lacks room (`useEditorPaneRoom`); a click overrides it. */
  readonly autoFolded?: boolean | undefined;
  readonly viewMode?: (typeof ViewMode)[keyof typeof ViewMode] | undefined;
  readonly renderConfigurationForm: (props: { applicationId: string | number | undefined; viewMode: (typeof ViewMode)[keyof typeof ViewMode] | undefined }) => ReactNode;
}

const containerSx = (isSmallWindow: boolean, collapsed: boolean): SxProps<Theme> => ({
  flex: 3,
  maxWidth: isSmallWindow ? '100%' : collapsed ? '1.75rem' : '20rem',
  minWidth: isSmallWindow ? '100%' : collapsed ? '1.75rem' : '20rem',
  height: !isSmallWindow ? '100%' : 'auto',
  position: 'relative',
  display: 'flex',
  flexDirection: 'column',
  boxSizing: 'border-box',
  gap: '1.5rem',
});
const collapseButtonSx: SxProps<Theme> = { padding: '0', marginLeft: '0', position: 'absolute', top: '0', right: '0', zIndex: 100 };

export function GeneralFormPanel({ applicationId, onCollapsed, autoFolded = false, viewMode, renderConfigurationForm }: GeneralFormPanelProps): ReactNode {
  const { isSmallWindow } = useIsSmallWindow();
  const { collapsed, toggle: onClickCollapsed } = usePaneFold(autoFolded, isSmallWindow, onCollapsed);

  return (
    <Box
      data-testid="pipeline-config-tab"
      sx={containerSx(isSmallWindow, collapsed)}
    >
      {!isSmallWindow && (
        <IconButton
          sx={collapseButtonSx}
          onClick={onClickCollapsed}
          // #135: icon-only, so without a name axe reports a critical
          // `button-name` (WCAG 4.1.2) failure and a screen reader announces
          // nothing but "button". The label tracks the state the click
          // produces, like every other collapse affordance in this app.
          aria-label={
            collapsed
              ? t('features.pipelines.generalFormPanel.expand', 'Expand the configuration panel')
              : t('features.pipelines.generalFormPanel.collapse', 'Collapse the configuration panel')
          }
        >
          {!collapsed ? <KeyboardDoubleArrowLeftIcon fontSize="small" /> : <KeyboardDoubleArrowRightIcon fontSize="small" />}
        </IconButton>
      )}
      {/*
        The save gate. Mounted UNCONDITIONALLY (its summary is what the
        collapse hides, not its veto) and here rather than in
        `ConfigurationTab.tsx` because that file sits one line under the
        §3.5 400-line budget. See `./settings/GraphAdmissionGate.tsx` for
        how a graph the native runtime would refuse disables Save.
      */}
      <GraphAdmissionGate summaryHidden={collapsed} />
      {!collapsed && renderConfigurationForm({ applicationId, viewMode })}
    </Box>
  );
}
