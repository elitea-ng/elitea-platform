/**
 * An answering agent's avatar with the default motion pack (ADR-0050
 * decision 6): the agent's icon (or, without one, the brand mark) as the
 * still, and one CSS motion per state from `MOTION_SPEC`
 * (`@elitea/client-core/conversation`) drawn over it. The desktop renders
 * the same web build.
 *
 * The motion's colours are theme tokens (ADR-0024), so a brand pack and the
 * dark scheme re-theme it. `motion="still"` draws the image alone; the list
 * decides it (one animated avatar per list, reduced motion).
 *
 * With an avatar pack (issue 1619), a state that has its own slot plays that
 * media (an image, or a muted looping <video> with its poster) and the
 * default motion is not drawn; a state with no slot shows the pack's still
 * with the default motion. `avatarSource` decides it.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';

import {
  MOTION_KIND,
  MOTION_SPEC,
  MOTION_TONE,
  avatarSource,
  type AvatarMotion,
  type MotionSpec,
  type MotionTone,
} from '@elitea/client-core/conversation';

import { BrandLogoMark } from '@/shared/ui/brand-logo';

export interface AgentAvatarProps {
  /** The agent's icon URL; absent shows the brand mark. */
  readonly iconUrl?: string | undefined;
  /** The version's `avatar_pack` as the wire sends it; its slots win over `iconUrl`. */
  readonly pack?: unknown;
  readonly motion?: AvatarMotion | undefined;
  /** A temporary sub-agent: a dashed ring (decision 7e). */
  readonly ephemeral?: boolean | undefined;
  /** Edge length, CSS. */
  readonly size?: string | undefined;
}

const DEFAULT_SIZE = '1.5rem';
const REDUCED_MOTION = '@media (prefers-reduced-motion: reduce)';

function toneColor(theme: Theme, tone: MotionTone | undefined): string {
  switch (tone) {
    case MOTION_TONE.Attention:
      return theme.vars.palette.warning.main;
    case MOTION_TONE.Success:
      return theme.vars.palette.success.main;
    case MOTION_TONE.Error:
      return theme.vars.palette.error.main;
    case MOTION_TONE.Accent:
    case undefined:
      return theme.vars.palette.primary.main;
  }
}

const KEYFRAMES = {
  '@keyframes elitea-avatar-breathe': { '0%, 100%': { transform: 'scale(1)' }, '50%': { transform: 'scale(var(--elitea-avatar-peak))' } },
  '@keyframes elitea-avatar-spin': { from: { transform: 'rotate(0deg)' }, to: { transform: 'rotate(360deg)' } },
  '@keyframes elitea-avatar-pulse': { '0%': { transform: 'scale(1)', opacity: 0.9 }, '100%': { transform: 'scale(var(--elitea-avatar-peak))', opacity: 0 } },
  '@keyframes elitea-avatar-halo': { '0%, 100%': { opacity: 0.35 }, '50%': { opacity: 1 } },
  '@keyframes elitea-avatar-pop': { '0%': { transform: 'scale(0)' }, '70%': { transform: 'scale(1.2)' }, '100%': { transform: 'scale(1)' } },
} as const;

function animation(name: string, spec: MotionSpec, timing = 'ease-in-out'): string {
  return `${name} ${spec.durationMs}ms ${timing} ${spec.loop ? 'infinite' : '1 forwards'}`;
}

/** The ring, pulse, orbit, halo or badge drawn over the still. */
function MotionLayer({ spec }: { readonly spec: MotionSpec }): ReactNode {
  const layer = (theme: Theme) => ({ position: 'absolute', inset: '-3px', borderRadius: theme.vars.shape.radiusPill, pointerEvents: 'none' }) as const;
  switch (spec.kind) {
    case MOTION_KIND.Breathe:
      return null;
    case MOTION_KIND.Ring:
      return (
        <Box
          data-testid="agent-avatar-motion"
          sx={(theme: Theme) => ({ ...layer(theme), border: '2px solid transparent', borderTopColor: toneColor(theme, spec.tone), borderRightColor: toneColor(theme, spec.tone), animation: animation('elitea-avatar-spin', spec, 'linear') })}
        />
      );
    case MOTION_KIND.Pulse:
      return (
        <Box
          data-testid="agent-avatar-motion"
          sx={(theme: Theme) => ({ ...layer(theme), inset: 0, border: `2px solid ${toneColor(theme, spec.tone)}`, animation: animation('elitea-avatar-pulse', spec, 'ease-out') })}
        />
      );
    case MOTION_KIND.Orbit:
      return (
        <Box data-testid="agent-avatar-motion" sx={(theme: Theme) => ({ ...layer(theme), animation: animation('elitea-avatar-spin', spec, 'linear') })}>
          <Box sx={(theme: Theme) => ({ position: 'absolute', top: 0, left: '50%', width: '5px', height: '5px', ml: '-2.5px', borderRadius: theme.vars.shape.radiusPill, background: toneColor(theme, spec.tone) })} />
        </Box>
      );
    case MOTION_KIND.Halo:
      return (
        <Box
          data-testid="agent-avatar-motion"
          sx={(theme: Theme) => ({ ...layer(theme), border: `2px solid ${toneColor(theme, spec.tone)}`, animation: animation('elitea-avatar-halo', spec) })}
        />
      );
    case MOTION_KIND.Badge:
      return (
        <Box
          data-testid="agent-avatar-motion"
          aria-hidden
          sx={(theme: Theme) => ({
            position: 'absolute', right: '-3px', bottom: '-3px', width: '0.75rem', height: '0.75rem', borderRadius: theme.vars.shape.radiusPill,
            display: 'flex', alignItems: 'center', justifyContent: 'center',
            background: toneColor(theme, spec.tone), color: theme.vars.palette.common.white,
            border: `1px solid ${theme.vars.palette.background.default}`,
            animation: animation('elitea-avatar-pop', spec, 'ease-out'),
          })}
        >
          <svg viewBox="0 0 12 12" width="8" height="8" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            {spec.badge === 'check' ? <path d="M2.5 6.5 5 9l4.5-6" /> : <path d="M3 3l6 6M9 3l-6 6" />}
          </svg>
        </Box>
      );
  }
}

/** The still with the motion of its state. */
export function AgentAvatar({ iconUrl, pack, motion = 'still', ephemeral = false, size = DEFAULT_SIZE }: AgentAvatarProps): ReactNode {
  const source = avatarSource(pack, iconUrl, motion);
  const spec = motion === 'still' || source?.ownMotion === true ? undefined : MOTION_SPEC[motion];
  const breathe = spec !== undefined && spec.peakScale > 1;
  return (
    <Box
      data-testid="chat-message-avatar"
      data-motion={motion}
      aria-hidden
      sx={{ ...KEYFRAMES, position: 'relative', width: size, height: size, minWidth: size, flexShrink: 0, [REDUCED_MOTION]: { '& *': { animation: 'none' } } }}
    >
      <Box
        sx={(theme: Theme) => ({
          width: '100%', height: '100%', borderRadius: theme.vars.shape.radiusPill, overflow: 'hidden',
          display: 'flex', alignItems: 'center', justifyContent: 'center',
          background: theme.vars.palette.background.aiParticipantIcon,
          ...(ephemeral ? { outline: `1px dashed ${theme.vars.palette.text.secondary}`, outlineOffset: '1px' } : {}),
          ...(breathe && spec.kind !== MOTION_KIND.Pulse
            ? { '--elitea-avatar-peak': String(spec.peakScale), animation: animation('elitea-avatar-breathe', spec) }
            : {}),
        })}
      >
        {source === undefined ? (
          <BrandLogoMark style={{ width: size, height: size }} />
        ) : source.kind === 'video' ? (
          // Never sound: muted, no controls; the poster shows before it plays.
          <Box
            component="video"
            data-testid="agent-avatar-video"
            src={source.src}
            poster={source.poster}
            autoPlay
            loop
            muted
            playsInline
            preload="metadata"
            sx={{ width: '100%', height: '100%', objectFit: 'cover' }}
          />
        ) : (
          <Box component="img" src={source.src} alt="" loading="lazy" sx={{ width: '100%', height: '100%', objectFit: 'cover' }} />
        )}
      </Box>
      {spec !== undefined && (
        <Box sx={{ position: 'absolute', inset: 0, '--elitea-avatar-peak': String(Math.max(spec.peakScale, 1.35)) }}>
          <MotionLayer spec={spec} />
        </Box>
      )}
    </Box>
  );
}
