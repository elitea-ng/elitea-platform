import type { ChangeEvent, ReactNode } from 'react';
import { useCallback } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import type { SxProps, Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { BaseSwitch } from '@/shared/ui/BaseSwitch';
import { InfoTooltip } from '@/shared/ui/InfoTooltip';
import { TextWithLink } from '@/shared/ui/TextWithLink';

import type { InternalToolInfoTooltip } from '../lib/internalTools';
import { INTERNAL_TOOL_ICONS } from '../lib/internalTools';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent/ui/agent-details/
 * configurations/switch/AgentInternalToolSwitch.jsx`.
 *
 * DISCLOSED REDESIGN — no ambient form context (see `../model/types.ts`'s
 * module doc comment): `checked`/`onCheckedChange` are explicit props
 * instead of reading/writing `version_details.meta.internal_tools` via
 * `useFormikContext()`; the caller (`ApplicationTools.tsx`) owns the array
 * membership computation and passes `checked` down per-instance.
 *
 * `EntityIcon` (`components/EntityIcon.jsx`, baseline) has no `shared/ui`
 * port and no confirmed home in this sub-unit's owned files — used across
 * 20 baseline call sites spanning multiple domains, so it belongs in
 * `shared/ui` once promoted, not duplicated here. This port renders the
 * resolved tool icon (`INTERNAL_TOOL_ICONS[icon]`, see `../lib/
 * internalTools.ts`) directly instead of routing through `EntityIcon`'s
 * `editable={false}` read-only path — the baseline's own read-only branch
 * of `EntityIcon` is exactly "render this icon in a small rounded box",
 * reproduced directly below with no loss of behaviour for this
 * non-editable call site.
 */
export interface AgentInternalToolSwitchProps {
  /**
   * The CANONICAL tool name (`INTERNAL_TOOLS_LIST[].name` — `attachments`,
   * `internal_mcp`, `pyodide`, …), i.e. the string this switch writes into
   * `version_details.meta.internal_tools`. It is what `data-testid` is built
   * from, so the handle a journey addresses a row by is the same identifier
   * the server stores; see the render below.
   */
  readonly name: string;
  readonly title: string;
  readonly icon: string;
  readonly checked: boolean;
  readonly onCheckedChange: (checked: boolean) => void;
  readonly disabled?: boolean | undefined;
  readonly infoTooltip?: InternalToolInfoTooltip | undefined;
  /**
   * Set when the CONFIGURED worker cannot run this tool (#865, #866;
   * `features/agents/lib/internalTools.ts`'s `AvailableInternalTool`,
   * `unavailableReason` — "Not available on the Rust worker"). The switch
   * renders disabled the same as a caller-supplied `disabled=true` would,
   * but wrapped in its own tooltip so the REASON is discoverable — a plain
   * `disabled` switch gives no indication of why, and this one specifically
   * is not a form-wide read-only state the user already understands.
   */
  readonly unavailableReason?: string | undefined;
}

export function AgentInternalToolSwitch({
  name,
  title,
  icon,
  checked,
  onCheckedChange,
  disabled,
  infoTooltip,
  unavailableReason,
}: AgentInternalToolSwitchProps): ReactNode {
  const onChange = useCallback(
    (_event: ChangeEvent<HTMLInputElement>, checkedValue: boolean) => {
      onCheckedChange(checkedValue);
    },
    [onCheckedChange],
  );

  const ToolIcon = INTERNAL_TOOL_ICONS[icon];

  return (
    // The testid is keyed by the CANONICAL tool `name`, not by the display
    // title. A row needs a handle at all because it is otherwise unnamed in
    // the accessibility tree (the FormControlLabel's label is empty) and a
    // journey has to pick one switch out of eight. Keying it off the title
    // made that handle a function of display COPY: renaming "Swarm Mode" or
    // translating the panel would silently move `internal-tool-swarm-mode`,
    // and the journey that toggles every tool would fail as "the switch does
    // not exist" — about a word, not about the feature. The name is the same
    // string the switch writes into `meta.internal_tools`, so the testid and
    // the stored value cannot drift apart.
    <Box
      sx={unavailableReason ? unavailableContainerSx : containerSx}
      data-testid={`internal-tool-${name}`}
    >
      <Box sx={contentContainerSx}>
        {ToolIcon && (
          <Box sx={entityIconSx}>
            <ToolIcon width="0.875rem" height="0.875rem" />
          </Box>
        )}
        <Typography
          variant="bodyMedium"
          color="text.secondary"
          sx={titleSx}
        >
          {title}
        </Typography>
        {infoTooltip && (
          <InfoTooltip
            title={
              infoTooltip.linkUrl && infoTooltip.linkText ? (
                <TextWithLink
                  text={infoTooltip.text}
                  linkUrl={infoTooltip.linkUrl}
                  linkText={infoTooltip.linkText}
                  suffix={infoTooltip.suffix}
                />
              ) : (
                infoTooltip.text
              )
            }
          />
        )}
      </Box>
      {unavailableReason ? (
        <Tooltip
          title={unavailableReason}
          placement="top"
        >
          {/* A disabled control fires no pointer events, so MUI's own
              guidance is to give the tooltip a non-disabled wrapper — this
              span — rather than anchor to the switch itself. */}
          <span>
            <FormControlLabel
              control={
                <BaseSwitch
                  checked={checked}
                  onChange={onChange}
                  disabled
                />
              }
              label=""
              sx={switchLabelSx}
            />
          </span>
        </Tooltip>
      ) : (
        <FormControlLabel
          control={
            <BaseSwitch
              checked={checked}
              onChange={onChange}
              disabled={disabled}
            />
          }
          label=""
          sx={switchLabelSx}
        />
      )}
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme: Theme) => ({
  backgroundColor: theme.vars.palette.background.userInputBackground,
  borderRadius: theme.vars.shape.radiusMd,
  height: '2.5rem',
  padding: '0.75rem',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: '0.5rem',
  width: '100%',
});

/** {@link containerSx}, dimmed — the "greyed" half of #865/#866's "greyed with a note" instead of the pre-existing silent-drop behaviour. */
const unavailableContainerSx: SxProps<Theme> = (theme: Theme) => ({
  backgroundColor: theme.vars.palette.background.userInputBackground,
  borderRadius: theme.vars.shape.radiusMd,
  height: '2.5rem',
  padding: '0.75rem',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: '0.5rem',
  width: '100%',
  opacity: 0.5,
});

const contentContainerSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '0.25rem',
  flex: 1,
  minWidth: 0,
};

const titleSx: SxProps<Theme> = {
  whiteSpace: 'nowrap',
  overflow: 'hidden',
  textOverflow: 'ellipsis',
};

const entityIconSx: SxProps<Theme> = (theme: Theme) => ({
  minWidth: '1.5rem',
  width: '1.5rem',
  height: '1.5rem',
  marginRight: '0.5rem',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  color: theme.vars.palette.secondary.main,
});

const switchLabelSx: SxProps<Theme> = {
  margin: 0,
  padding: 0,
};
