/**
 * The desktop composer's footer parts, shared by the chat (`widgets/chat-box`)
 * and a Local work thread (`pages/workspace`) so both read as one composer:
 *
 * - `ComposerIconButton`: a plain 28px icon, no fill or rim (attach, mic).
 * - `ComposerPicker`: a choice shown as its value and a chevron ("Act ⌄",
 *   "Qwen3.8 ⌄"), in text, the way a native toolbar shows a pop-up button;
 *   the label truncates before the row overflows.
 * - `ComposerMoreMenu`: the ⋯ menu for what is used less (clear history,
 *   model settings, read aloud, step limit), its rows from `native-menu`.
 * - `ComposerSendButton`: the one round accent button at the end.
 *
 * The row itself: `ComposerRow`, a leading group that shrinks and a trailing
 * group that does not, so Send never leaves the window.
 */
import { useEffect, useRef, useState, type ReactNode } from "react";

import ArrowUpwardIcon from "@mui/icons-material/ArrowUpward";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";
import MoreHorizIcon from "@mui/icons-material/MoreHoriz";
import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import IconButton, { type IconButtonProps } from "@mui/material/IconButton";
import Menu from "@mui/material/Menu";
import type { PopoverActions } from "@mui/material/Popover";
import type { Theme } from "@mui/material/styles";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";

import { t } from "@/shared/i18n";

const hover = (theme: Theme) => ({
  background: theme.vars.palette.background.button.drawerMenu.hover,
});

/**
 * MUI places a menu once, when it opens; a menu whose rows change in place
 * (Step limit › turns into its choices) would then grow off the window.
 * This re-places it whenever its paper changes size.
 */
function useFollowContent(open: boolean): {
  action: React.RefObject<PopoverActions | null>;
  paperRef: (node: HTMLDivElement | null) => void;
} {
  const action = useRef<PopoverActions | null>(null);
  const [paper, setPaper] = useState<HTMLDivElement | null>(null);
  useEffect(() => {
    if (!open || paper === null || typeof ResizeObserver === "undefined")
      return undefined;
    const observer = new ResizeObserver(() => action.current?.updatePosition());
    observer.observe(paper);
    return () => observer.disconnect();
  }, [open, paper]);
  return { action, paperRef: setPaper };
}

export interface ComposerIconButtonProps extends Omit<
  IconButtonProps,
  "size" | "children" | "title"
> {
  readonly label: string;
  readonly children: ReactNode;
  /** On: a toggle that is on (recording, read aloud). */
  readonly pressed?: boolean;
}

export function ComposerIconButton({
  label,
  children,
  pressed,
  ...props
}: ComposerIconButtonProps): React.JSX.Element {
  const button = (
    <IconButton
      {...props}
      aria-label={label}
      aria-pressed={pressed}
      sx={(theme: Theme) => ({
        width: "1.75rem",
        height: "1.75rem",
        padding: 0,
        borderRadius: theme.vars.shape.radiusSm,
        color:
          pressed === true
            ? theme.vars.palette.primary.main
            : theme.vars.palette.text.metrics,
        "& svg": { width: "1.125rem", height: "1.125rem" },
        "&:hover": hover(theme),
      })}
    >
      {children}
    </IconButton>
  );
  return (
    <Tooltip title={label}>
      {/* A disabled button fires no events: the span keeps the tooltip (an admin-disabled mic says why). */}
      {props.disabled === true ? (
        <Box component="span" sx={{ display: "inline-flex" }}>
          {button}
        </Box>
      ) : (
        button
      )}
    </Tooltip>
  );
}

export interface ComposerPickerProps {
  /** What is chosen, as shown in the row. */
  readonly value: ReactNode;
  /** What the choice is about, for the tooltip and screen readers ("Model", "Mode"). */
  readonly label: string;
  /** The menu's rows (`MenuRow`s); called with `close` so a row can close the menu. */
  readonly children: (close: () => void) => ReactNode;
  readonly icon?: ReactNode;
  readonly testId?: string;
  readonly disabled?: boolean;
  /** The menu's least width; default the button's. */
  readonly minMenuWidth?: string;
}

export function ComposerPicker({
  value,
  label,
  children,
  icon,
  testId,
  disabled,
  minMenuWidth = "12rem",
}: ComposerPickerProps): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const close = (): void => setAnchor(null);
  const follow = useFollowContent(anchor !== null);
  return (
    <>
      <Tooltip title={label}>
        {/* The span holds the tooltip when the button is disabled (it fires no events then), and shrinks with it. */}
        <Box
          component="span"
          sx={{ display: "inline-flex", minWidth: 0, flexShrink: 1 }}
        >
          <ButtonBase
            data-testid={testId}
            disabled={disabled}
            aria-haspopup="menu"
            aria-expanded={anchor !== null}
            aria-label={
              typeof value === "string" ? `${label}: ${value}` : label
            }
            onClick={(event) => setAnchor(event.currentTarget)}
            sx={(theme: Theme) => ({
              minWidth: 0,
              flexShrink: 1,
              gap: 0.5,
              height: "1.75rem",
              paddingX: 0.75,
              borderRadius: theme.vars.shape.radiusSm,
              color: theme.vars.palette.text.secondary,
              "& svg": {
                width: "1rem",
                height: "1rem",
                flexShrink: 0,
                color: theme.vars.palette.text.metrics,
              },
              "&:hover": hover(theme),
              '&[aria-expanded="true"]': {
                background:
                  theme.vars.palette.background.button.drawerMenu.selected,
              },
            })}
          >
            {icon}
            <Typography
              variant="labelSmall"
              component="span"
              noWrap
              sx={{ minWidth: 0 }}
            >
              {value}
            </Typography>
            <ExpandMoreIcon aria-hidden />
          </ButtonBase>
        </Box>
      </Tooltip>
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={close}
        action={follow.action}
        anchorOrigin={{ vertical: "top", horizontal: "left" }}
        transformOrigin={{ vertical: "bottom", horizontal: "left" }}
        slotProps={{
          list: { dense: true, "aria-label": label },
          paper: {
            ref: follow.paperRef,
            sx: { minWidth: minMenuWidth, marginTop: -0.5 },
          },
        }}
      >
        {children(close)}
      </Menu>
    </>
  );
}

export function ComposerMoreMenu({
  children,
  testId = "composer-more",
}: {
  readonly children: (close: () => void) => ReactNode;
  readonly testId?: string;
}): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const close = (): void => setAnchor(null);
  const label = t("chatInput.more", "More");
  const follow = useFollowContent(anchor !== null);
  return (
    <>
      <ComposerIconButton
        label={label}
        data-testid={testId}
        aria-haspopup="menu"
        aria-expanded={anchor !== null}
        onClick={(event) => setAnchor(event.currentTarget)}
      >
        <MoreHorizIcon />
      </ComposerIconButton>
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={close}
        action={follow.action}
        anchorOrigin={{ vertical: "top", horizontal: "left" }}
        transformOrigin={{ vertical: "bottom", horizontal: "left" }}
        slotProps={{
          list: { dense: true, "aria-label": label },
          paper: {
            ref: follow.paperRef,
            sx: { minWidth: "14rem", marginTop: -0.5 },
          },
        }}
      >
        {children(close)}
      </Menu>
    </>
  );
}

export interface ComposerSendButtonProps {
  readonly label: string;
  readonly disabled: boolean;
  readonly onClick: () => void;
  readonly testId?: string;
  /** Another icon in the same button (Stop's square, speaking mode's wave). */
  readonly icon?: ReactNode;
}

export function ComposerSendButton({
  label,
  disabled,
  onClick,
  testId,
  icon,
}: ComposerSendButtonProps): React.JSX.Element {
  return (
    <Tooltip title={label}>
      {/* A disabled button fires no events: the span keeps the tooltip. */}
      <span>
        <IconButton
          data-testid={testId}
          aria-label={label}
          disabled={disabled}
          onClick={onClick}
          sx={(theme: Theme) => ({
            width: "1.75rem",
            height: "1.75rem",
            padding: 0,
            borderRadius: theme.vars.shape.radiusPill,
            background: theme.vars.palette.primary.main,
            color: theme.vars.palette.primary.contrastText,
            "& svg": { width: "1rem", height: "1rem" },
            "&:hover": {
              background: theme.vars.palette.primary.main,
              filter: "brightness(1.08)",
            },
            "&:disabled": {
              background: theme.vars.palette.action.disabledBackground,
              color: theme.vars.palette.text.metrics,
            },
          })}
        >
          {icon ?? <ArrowUpwardIcon />}
        </IconButton>
      </span>
    </Tooltip>
  );
}

/** The footer row: what it leads with shrinks (labels truncate), what it ends with keeps its room. */
export function ComposerRow({
  leading,
  trailing,
}: {
  readonly leading: ReactNode;
  readonly trailing: ReactNode;
}): React.JSX.Element {
  return (
    <Box
      sx={{
        display: "flex",
        alignItems: "center",
        gap: 0.5,
        width: "100%",
        minWidth: 0,
      }}
    >
      <Box
        sx={{
          display: "flex",
          alignItems: "center",
          gap: 0.25,
          flex: 1,
          minWidth: 0,
          overflow: "hidden",
        }}
      >
        {leading}
      </Box>
      <Box
        sx={{ display: "flex", alignItems: "center", gap: 0.5, flexShrink: 0 }}
      >
        {trailing}
      </Box>
    </Box>
  );
}
