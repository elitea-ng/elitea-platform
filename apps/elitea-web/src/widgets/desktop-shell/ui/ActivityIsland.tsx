/**
 * The activity island: a capsule centred in the toolbar that says what the
 * app is doing — an approval waiting, a turn running (phase or tool, a timer,
 * steps), a commit, a send to the cloud, a thread in the cloud, an index
 * build — most important first. Hover or click opens the list of everything
 * running, with progress and a cancel where the host has one. Nothing
 * running shows the idle line the page gives it, if any, or nothing at all.
 *
 * It animates its width as the text changes, and keeps still under reduced
 * motion.
 */
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';

import ClickAwayListener from '@mui/material/ClickAwayListener';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import LinearProgress from '@mui/material/LinearProgress';
import Paper from '@mui/material/Paper';
import Popper from '@mui/material/Popper';
import type { Theme } from '@mui/material/styles';
import useMediaQuery from '@mui/material/useMediaQuery';
import StopCircleOutlinedIcon from '@mui/icons-material/StopCircleOutlined';

import { useWorkspaceIpc } from '@/features/workspace';
import { useHandoffIpc } from '@/features/workspace-handoff';
import { useIndexIpc } from '@/features/workspace-index';
import { t } from '@/shared/i18n';

import { buildActivities, formatElapsed, type Activity } from '../lib/activityModel';
import { useActivityFeed } from '../model/activityFeed.store';
import { useShellActions } from '../model/useShellActions';

/** Re-render each second while something has a timer. */
function useNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return undefined;
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [active]);
  return now;
}

function useActivities(): Activity[] {
  const turns = useActivityFeed((s) => s.turns);
  const indexes = useActivityFeed((s) => s.indexes);
  const sends = useActivityFeed((s) => s.sends);
  const cloud = useActivityFeed((s) => s.cloud);
  const cloudRuns = useActivityFeed((s) => s.cloudRuns);
  return useMemo(() => buildActivities({ turns, indexes, sends, cloud, cloudRuns }), [turns, indexes, sends, cloud, cloudRuns]);
}

const tonePulse = { '@keyframes elitea-island-pulse': { '0%, 100%': { opacity: 1 }, '50%': { opacity: 0.35 } } } as const;

function Leading({ activity, reduced }: { activity: Activity | null; reduced: boolean }): React.JSX.Element | null {
  if (activity === null) return null;
  if (activity.tone === 'attention' || activity.tone === 'warn') {
    return (
      <Box
        aria-hidden
        sx={(theme: Theme) => ({
          width: 7,
          height: 7,
          borderRadius: '50%',
          flexShrink: 0,
          background: activity.tone === 'warn' ? theme.vars.palette.warning.main : theme.vars.palette.primary.main,
          ...tonePulse,
          animation: reduced || activity.tone === 'warn' ? 'none' : 'elitea-island-pulse 1.4s ease-in-out infinite',
        })}
      />
    );
  }
  return <CircularProgress aria-hidden size={11} thickness={6} variant={activity.progress === null ? 'indeterminate' : 'determinate'} value={activity.progress === null ? 0 : (activity.progress.done / Math.max(activity.progress.total, 1)) * 100} sx={{ flexShrink: 0, ...(reduced ? { animation: 'none' } : {}) }} />;
}

/** The capsule's text: the primary activity's label and timer, or the idle line. */
function capsuleText(primary: Activity | null, idle: string | null, now: number): { label: string; timer: string } {
  if (primary === null) return { label: idle ?? '', timer: '' };
  return { label: primary.label, timer: primary.since === null ? '' : formatElapsed(now - primary.since) };
}

function RowActions({ activity, onFocus }: { activity: Activity; onFocus: (a: Activity) => void }): React.JSX.Element {
  const workspaceIpc = useWorkspaceIpc();
  const indexIpc = useIndexIpc();
  const handoffIpc = useHandoffIpc();
  const cancel = (): void => {
    const key = activity.cancelKey;
    if (key === null) return;
    if (activity.kind === 'turn' || activity.kind === 'approval') void workspaceIpc?.cancelTurn(key).catch(() => undefined);
    else if (activity.kind === 'index') void indexIpc?.cancel(key).catch(() => undefined);
    else if (activity.kind === 'handoff') void handoffIpc?.cancel(key).catch(() => undefined);
  };
  const cancellable = activity.cancelKey !== null && activity.kind !== 'cloud' && activity.kind !== 'commit';
  return (
    <>
      {activity.kind === 'approval' && activity.workspaceId !== null && (
        <ButtonBase
          onClick={() => onFocus(activity)}
          sx={(theme: Theme) => ({ fontSize: '0.6875rem', fontWeight: 600, color: theme.vars.palette.primary.main, padding: '1px 6px', borderRadius: '5px' })}
        >
          {t('desktop.island.review', 'Review')}
        </ButtonBase>
      )}
      {cancellable && (
        <IconButton size="small" aria-label={t('desktop.island.cancel', 'Cancel {{what}}', { what: activity.label })} onClick={cancel}>
          <StopCircleOutlinedIcon sx={{ fontSize: 16 }} />
        </IconButton>
      )}
    </>
  );
}

function ActivityRow({ activity, now, reduced, onFocus }: { activity: Activity; now: number; reduced: boolean; onFocus: (a: Activity) => void }): React.JSX.Element {
  const clickable = activity.workspaceId !== null;
  return (
    <Box data-testid="island-row" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, paddingY: 0.75 }}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
        <Leading activity={activity} reduced={reduced} />
        <ButtonBase
          disabled={!clickable}
          onClick={() => onFocus(activity)}
          sx={{ flex: 1, minWidth: 0, flexDirection: 'column', alignItems: 'flex-start', textAlign: 'left' }}
        >
          <Box component="span" sx={{ fontSize: '0.75rem', fontWeight: 600, maxWidth: '100%', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
            {activity.label}
          </Box>
          {activity.detail !== '' && (
            <Box component="span" sx={(theme: Theme) => ({ fontSize: '0.6875rem', color: theme.vars.palette.text.secondary, maxWidth: '100%', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' })}>
              {activity.detail}
            </Box>
          )}
        </ButtonBase>
        {activity.since !== null && (
          <Box component="span" sx={(theme: Theme) => ({ fontSize: '0.6875rem', fontVariantNumeric: 'tabular-nums', color: theme.vars.palette.text.secondary })}>
            {formatElapsed(now - activity.since)}
          </Box>
        )}
        <RowActions activity={activity} onFocus={onFocus} />
      </Box>
      {activity.progress !== null && (
        <LinearProgress
          variant="determinate"
          value={Math.round((activity.progress.done / Math.max(activity.progress.total, 1)) * 100)}
          aria-label={activity.label}
          sx={{ height: 3, borderRadius: 2 }}
        />
      )}
    </Box>
  );
}

export interface ActivityIslandProps {
  /** What to say when nothing runs ("Index ready · 12.4K entities"); nothing shows without it. */
  idle?: string | null;
}

/** Hover (with a small grace delay) or a click pins the popover open. */
function usePopoverState(): { open: (has: boolean) => boolean; enter: () => void; leave: () => void; toggle: () => void; close: () => void } {
  const [hover, setHover] = useState(false);
  const [pinned, setPinned] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);
  return {
    open: (has) => has && (hover || pinned),
    enter: () => {
      window.clearTimeout(timer.current);
      setHover(true);
    },
    leave: () => {
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setHover(false), 200);
    },
    toggle: () => setPinned((value) => !value),
    close: () => {
      setPinned(false);
      setHover(false);
    },
  };
}

const visuallyHidden = { position: 'absolute', width: 1, height: 1, overflow: 'hidden', clip: 'rect(0 0 0 0)', whiteSpace: 'nowrap' } as const;

/** An approval opens its thread; one with no known folder (a turn from before this window listened) opens the list instead. */
const canFocus = (activity: Activity): boolean => activity.kind === 'approval' && activity.workspaceId !== null;

const nothingToSay = (primary: Activity | undefined, idle: string | null): boolean => primary === undefined && (idle === null || idle === '');

/** Opens the folder and thread an activity belongs to. */
function useFocusActivity(done: () => void): (activity: Activity) => void {
  const run = useShellActions();
  return (activity) => {
    if (activity.workspaceId === null) return;
    done();
    run({ type: 'open_workspace', workspaceId: activity.workspaceId, ...(activity.conversationId === null ? {} : { conversationId: activity.conversationId }) });
  };
}

/** The capsule's width follows its content, so it can animate between texts. */
function useContentWidth(signature: string): { inner: React.RefObject<HTMLSpanElement | null>; width: number | undefined } {
  const inner = useRef<HTMLSpanElement>(null);
  const [width, setWidth] = useState<number | undefined>(undefined);
  useLayoutEffect(() => {
    const node = inner.current;
    if (node === null) return undefined;
    setWidth(node.getBoundingClientRect().width);
    if (typeof ResizeObserver === 'undefined') return undefined;
    const observer = new ResizeObserver(() => setWidth(node.getBoundingClientRect().width));
    observer.observe(node);
    return () => observer.disconnect();
  }, [signature]);
  return { inner, width };
}

function Muted({ children }: { children: string }): React.JSX.Element {
  return (
    <Box component="span" aria-hidden sx={(theme: Theme) => ({ fontVariantNumeric: 'tabular-nums', color: theme.vars.palette.text.secondary })}>
      {children}
    </Box>
  );
}

function capsuleSx(theme: Theme, { width, reduced, attention }: { width: number | undefined; reduced: boolean; attention: boolean }) {
  const ring = attention ? `color-mix(in srgb, ${theme.vars.palette.primary.main} 45%, transparent)` : `color-mix(in srgb, ${theme.vars.palette.text.primary} 14%, transparent)`;
  return {
    height: 26,
    maxWidth: '100%',
    borderRadius: '13px',
    overflow: 'hidden',
    width,
    transition: reduced ? 'none' : 'width 280ms cubic-bezier(0.2, 0.8, 0.2, 1), background-color 160ms',
    background: attention ? `color-mix(in srgb, ${theme.vars.palette.primary.main} 16%, transparent)` : `color-mix(in srgb, ${theme.vars.palette.text.primary} 7%, transparent)`,
    boxShadow: `inset 0 0 0 0.5px ${ring}`,
    '&:hover': { background: `color-mix(in srgb, ${theme.vars.palette.text.primary} 11%, transparent)` },
  };
}

const popoverSx = (theme: Theme) => ({
  width: 320,
  maxHeight: 360,
  overflowY: 'auto',
  paddingX: 1.5,
  paddingY: 0.5,
  borderRadius: '10px',
  background: theme.vars.palette.background.paper,
  backgroundImage: 'none',
  border: `0.5px solid ${theme.vars.palette.divider}`,
  boxShadow: `0 8px 28px color-mix(in srgb, ${theme.vars.palette.text.primary} 22%, transparent)`,
});

export function ActivityIsland({ idle = null }: ActivityIslandProps): React.JSX.Element | null {
  const activities = useActivities();
  const reduced = useMediaQuery('(prefers-reduced-motion: reduce)');
  const [primary] = activities;
  const now = useNow(activities.some((a) => a.since !== null));
  const popover = usePopoverState();
  const anchor = useRef<HTMLButtonElement>(null);
  const { label, timer } = capsuleText(primary ?? null, idle, now);
  const { inner, width } = useContentWidth(`${label}|${timer}|${String(activities.length)}`);

  const focus = useFocusActivity(popover.close);

  if (nothingToSay(primary, idle)) return null;
  const open = popover.open(activities.length > 0);
  const attention = primary?.tone === 'attention';

  return (
    <ClickAwayListener onClickAway={popover.close}>
      <Box sx={{ display: 'inline-flex' }} onMouseEnter={popover.enter} onMouseLeave={popover.leave}>
        <ButtonBase
          ref={anchor}
          data-testid="activity-island"
          data-kind={primary?.kind ?? 'idle'}
          aria-expanded={activities.length > 0 ? open : undefined}
          onClick={() => (primary !== undefined && canFocus(primary) ? focus(primary) : popover.toggle())}
          sx={(theme: Theme) => capsuleSx(theme, { width, reduced, attention })}
        >
          <Box ref={inner} component="span" sx={{ display: 'inline-flex', alignItems: 'center', gap: 0.75, paddingX: 1.25, whiteSpace: 'nowrap', fontSize: '0.75rem', fontWeight: 500 }}>
            <Leading activity={primary ?? null} reduced={reduced} />
            <span>{label}</span>
            {timer !== '' && <Muted>{timer}</Muted>}
            {activities.length > 1 && <Muted>{`+${String(activities.length - 1)}`}</Muted>}
          </Box>
          {/* Announced when the state or the label changes — never the ticking timer. */}
          <Box component="output" aria-live="polite" sx={visuallyHidden}>
            {label}
          </Box>
        </ButtonBase>
        <Popper open={open} anchorEl={anchor.current} placement="bottom" sx={{ zIndex: (theme: Theme) => theme.zIndex.modal }} modifiers={[{ name: 'offset', options: { offset: [0, 6] } }]}>
          <Paper
            component="section"
            aria-label={t('desktop.island.title', 'Activity')}
            data-testid="island-popover"
            onMouseEnter={popover.enter}
            onMouseLeave={popover.leave}
            sx={popoverSx}
          >
            {activities.map((activity) => (
              <ActivityRow key={activity.id} activity={activity} now={now} reduced={reduced} onFocus={focus} />
            ))}
          </Paper>
        </Popper>
      </Box>
    </ClickAwayListener>
  );
}
