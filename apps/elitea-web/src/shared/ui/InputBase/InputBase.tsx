import type { ReactNode } from 'react';
import { useCallback, useId, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import MuiTextField, { type TextFieldProps } from '@mui/material/TextField';

import { InfoLabelWithTooltip } from '../InfoLabelWithTooltip';
import { InputActionsToolbar } from '../InputActionsToolbar';
import { combineSx } from '../lib/combineSx';

/** @public */
export interface InputBaseActionsOptions {
  /** Master switch for the hover-revealed toolbar; `false`/undefined never renders it. */
  enabled?: boolean;
  showCopy?: boolean;
  /** Only takes effect when `expand` is also set — there is nothing to expand otherwise. */
  showExpand?: boolean;
  showFullScreen?: boolean;
  /** Keeps the toolbar visible without a real hover — for Storybook/tests. */
  forceShow?: boolean;
}

/** @public */
export interface InputBaseExpandOptions {
  /** Starts at `minRows` instead of `maxRows`. */
  collapsed?: boolean;
  minRows?: number;
  maxRows?: number;
}

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface InputBaseProps
  extends Omit<TextFieldProps, 'variant' | 'label' | 'multiline' | 'minRows' | 'maxRows' | 'onCopy'> {
  label?: ReactNode;
  /** Renders `label` (string labels only) with an info-icon tooltip next to it. */
  tooltipDescription?: ReactNode;
  actions?: InputBaseActionsOptions;
  /** Presence alone makes the field multiline; omit for a single-line input. */
  expand?: InputBaseExpandOptions;
  /**
   * Renders the OUTLINED shape — a bordered, rounded box with the label
   * moved out above it — instead of the default underline.
   *
   * The baseline's `variantInput="outlined"`
   * (`[fsd]/shared/ui/input/InputBase.jsx:186`). Two screens in Settings ask
   * for it and neither could get it: Memory's "Summarization instructions"
   * and AI Personality's "User instructions" are both large free-text areas
   * that the production UI draws as a panel. With `variant` hard-coded to
   * `"standard"` here they rendered as a bare underline under a floating
   * label, so a three-row textarea read as an empty gap.
   */
  outlined?: boolean;
  /**
   * Fires after the copy action's `navigator.clipboard.writeText` settles —
   * `error` is present only on rejection. Replaces the baseline's direct
   * `useToast()` call, which `shared/ui` cannot import (props/callbacks
   * only, per this unit's layering rule); the caller decides how to surface
   * the result.
   */
  onCopy?: (value: string, error?: unknown) => void;
  onFullScreen?: () => void;
  containerSx?: SxProps<Theme>;
}

const containerStyles = {
  position: 'relative' as const,
  display: 'flex',
  flexDirection: 'column' as const,
};

/** Baseline `styledInputBaseStyles.outlinedLabelRow`. */
const outlinedLabelRowSx = {
  display: 'flex',
  justifyContent: 'space-between',
  alignItems: 'center',
  marginBottom: '0.25rem',
  minHeight: '1.75rem',
  width: '100%',
  paddingLeft: '0.75rem',
};

function toolbarPositionSx(hasLabel: boolean) {
  return (theme: Theme) => ({
    position: 'absolute' as const,
    display: 'flex',
    top: hasLabel ? theme.spacing(0.25) : theme.spacing(-2.5),
    right: theme.spacing(1.5),
    zIndex: 1,
  });
}

function initialRows(expand: InputBaseExpandOptions | undefined): number | undefined {
  if (!expand) return undefined;
  return expand.collapsed ? (expand.minRows ?? 3) : expand.maxRows;
}

interface ResolvedActions {
  enabled: boolean;
  forceShow: boolean;
  showCopy: boolean;
  showExpand: boolean;
  showFullScreen: boolean;
}

/**
 * Collapses every `actions?.x ?? default` read into one call — each
 * optional-chain/nullish-coalescing operator is its own branch for
 * `complexity` (oxlint's native rule, §3.5), so folding five of them
 * straight into `InputBase`'s body was what pushed it to 16 (budget 12).
 * Splitting this out, the same way `BaseModal` splits `ModalHeader`/
 * `ModalActions` into their own functions, is a complexity-budget fix, not
 * a behaviour change.
 */
function resolveActions(actions: InputBaseActionsOptions | undefined, hasExpand: boolean): ResolvedActions {
  return {
    enabled: actions?.enabled ?? false,
    forceShow: actions?.forceShow ?? false,
    showCopy: actions?.showCopy ?? true,
    showExpand: (actions?.showExpand ?? true) && hasExpand,
    showFullScreen: actions?.showFullScreen ?? true,
  };
}

/** Whether the outlined layout has a label row to render above the field. */
function hasOutlinedLabelRow(outlined: boolean, labelNode: ReactNode): boolean {
  if (!outlined) return false;
  return labelNode !== undefined && labelNode !== null && labelNode !== '';
}

function renderLabel(label: ReactNode, tooltipDescription: ReactNode | undefined): ReactNode {
  if (typeof label !== 'string' || tooltipDescription === undefined) return label;
  return (
    <InfoLabelWithTooltip
      label={label}
      tooltip={tooltipDescription}
      inline
    />
  );
}

/**
 * A `TextField` wrapper adding a label-with-tooltip slot and an optional
 * hover-revealed actions toolbar (copy / expand-collapse / full-screen).
 * Ported from `apps/elitea-ui/src/[fsd]/shared/ui/input/InputBase.jsx`.
 *
 * Dropped from the baseline, deliberately (§3.5 12-prop budget — the
 * baseline was 30 flat props; the essentials above are grouped into
 * `actions`/`expand` option objects the way `BaseModal` groups
 * `header`/`actions`):
 *  - `editswitcher`/`editswitchconfig` — a read-only/edit-mode overlay
 *    driven by app-level state; no current `shared/ui` caller and nothing
 *    in this layer to drive it from (props/callbacks only).
 *  - `enableAutoBlur` (`useAutoBlur()`) — an app-level hook `shared/ui`
 *    cannot import here; a caller that needs it can call `onBlur` itself.
 *  - The baseline's collapsed-text `-webkit-line-clamp` styling reached the
 *    `.MuiInputBase-input` class directly, which `elitea/no-mui-internal-selector`
 *    (R-T6) now bans outside `shared/brand/mui-overrides/`. `expand.minRows`/
 *    `maxRows` drive MUI's own `multiline`/`maxRows` row-limiting instead —
 *    a real row cap, not a CSS clamp hack.
 */
export function InputBase({
  label,
  tooltipDescription,
  actions,
  expand,
  outlined = false,
  onCopy,
  onFullScreen,
  containerSx,
  value,
  sx,
  ...rest
}: InputBaseProps): ReactNode {
  const generatedId = useId();
  const outlinedLabelId = `input-base-${generatedId}-label`;
  const [isHovering, setIsHovering] = useState(false);
  const [rows, setRows] = useState<number | undefined>(() => initialRows(expand));

  const toggleExpand = useCallback(() => {
    setRows((prev) => (prev === expand?.maxRows ? (expand?.minRows ?? 3) : expand?.maxRows));
  }, [expand]);

  const handleCopy = useCallback(() => {
    const text = typeof value === 'string' ? value : '';
    void navigator.clipboard.writeText(text).then(
      () => onCopy?.(text),
      (error: unknown) => onCopy?.(text, error),
    );
  }, [value, onCopy]);

  const resolved = resolveActions(actions, expand !== undefined);
  const showToolbar = resolved.enabled && (isHovering || resolved.forceShow);
  const isExpanded = expand !== undefined && rows === expand.maxRows;
  const toolbarValue = typeof value === 'string' ? value : undefined;
  const minRows = expand !== undefined ? expand.minRows : undefined;
  const labelNode = renderLabel(label, tooltipDescription);

  return (
    <Box
      onMouseEnter={() => setIsHovering(true)}
      onMouseLeave={() => setIsHovering(false)}
      sx={combineSx(containerStyles, containerSx)}
    >
      {/* The label row carries an id and the field points at it with
        * `aria-labelledby`. Moving the label OUT of the `TextField` (which is
        * what the outlined shape is) otherwise leaves the textarea with no
        * accessible name at all — `getByLabelText('Summarization
        * instructions')` stops resolving, and so does a screen reader. */}
      {hasOutlinedLabelRow(outlined, labelNode) && (
        <Box id={outlinedLabelId} sx={outlinedLabelRowSx}>
          {labelNode}
        </Box>
      )}
      {showToolbar && (
        <InputActionsToolbar
          value={toolbarValue}
          showCopyAction={resolved.showCopy}
          showExpandAction={resolved.showExpand}
          showFullScreenAction={resolved.showFullScreen}
          isExpanded={isExpanded}
          onCopy={handleCopy}
          onToggleExpand={toggleExpand}
          onFullScreen={onFullScreen}
          sx={toolbarPositionSx(Boolean(label))}
        />
      )}
      <MuiTextField
        variant={outlined ? 'outlined' : 'standard'}
        fullWidth
        value={value}
        label={outlined ? undefined : labelNode}
        multiline={expand !== undefined}
        minRows={minRows}
        maxRows={rows}
        sx={sx}
        {...(hasOutlinedLabelRow(outlined, labelNode)
          ? { slotProps: { htmlInput: { 'aria-labelledby': outlinedLabelId } } }
          : {})}
        {...rest}
      />
    </Box>
  );
}
