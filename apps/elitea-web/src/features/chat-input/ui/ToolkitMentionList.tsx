/**
 * A local replacement for the baseline's toolkit-phase row renderer
 * (`SlashSuggestionList.jsx`'s `NewParticipantList` import + its
 * `NewParticipantCard`). `NewParticipantList` is `pages/NewChat/
 * Recommendations/NewParticipantList.jsx` — a PAGE-layer component;
 * `features/chat-input` cannot import from `pages/` (pages/ sits ABOVE
 * features/ in the FSD layer order). This is the toolkit-phase equivalent
 * of `shared/ui/MentionToolList` (the "/" system's tool-phase list,
 * reused as-is by `SlashSuggestionList.tsx` — see that file's own doc
 * comment) — ported from what `NewParticipantCard.jsx` actually renders
 * for a toolkit row, not the page component itself.
 *
 * **Layout simplification (disclosed).** The baseline's `NewParticipantList`
 * is a responsive MULTI-COLUMN card grid (`theme.breakpoints` across ten
 * `prompt_list_*` rungs — real tokens in this app's theme too, see
 * `shared/brand/buildTheme.ts`'s `BREAKPOINT_VALUES`), designed for the
 * full-page "New Chat" recommendation view. This component instead renders
 * a single-column scrollable list, matching `shared/ui/MentionToolList`'s
 * own styling (the sibling "tool"-phase list `SlashSuggestionList.tsx`
 * renders right next to this one) — both are narrow inline popovers
 * anchored to the chat textarea, not a wide recommendation page, so a
 * consistent single-column treatment fits the real use site better than
 * reproducing a ten-breakpoint responsive grid built for a different one.
 * Also dropped, matching the baseline's OWN call site
 * (`SlashSuggestionList.jsx` always passes `isLoading={false}
 * isFetching={false} existingParticipantUids={[]}`): loading skeletons and
 * the "already added" badge — neither is ever exercised for the "/"
 * toolkit dropdown.
 *
 * **Icon resolution — disclosed, established scope reduction**, same as
 * `../lib/hooks/useSlashMention.ts`'s own doc comment: `entitySettings
 * .iconMeta.url` if present, else a generic toolkit/MCP glyph (never the
 * baseline's ~30-brand-icon `getToolIconByType`).
 */
import type { ReactNode } from 'react';
import { useEffect, useRef } from 'react';

import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import ClickAwayListener from '@mui/material/ClickAwayListener';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { GradientIconWrapper } from '@/shared/ui/GradientIconWrapper';
import { McpIcon } from '@/shared/ui/icons/mcp-icon';
import { SkillIcon } from '@/shared/ui/icons/skill-icon';
import { ToolIcon } from '@/shared/ui/icons/tool-icon';

import type { SlashParticipantToolkit } from '../lib/hooks/useSlashMention';

/** `mcp.helpers.js:7-14`'s `isMcpToolkitType`, duplicated a 3rd time in this slice for the same reason `useSlashMention.ts`'s own copy is — see that file's header. */
function isMcpToolkitType(type: string): boolean {
  return type === 'mcp' || type.startsWith('mcp_');
}

const iconGlyphSx = (theme: Theme) => ({ color: theme.vars.palette.icon.fill.default, width: '1.25rem', height: '1.25rem' });

function ToolkitMentionIcon({ toolkit }: { readonly toolkit: SlashParticipantToolkit }): ReactNode {
  if (toolkit.iconUrl) {
    return (
      <Box
        component="img"
        src={toolkit.iconUrl}
        alt=""
        sx={{ width: '2.25rem', height: '2.25rem', minWidth: '2.25rem', borderRadius: (theme: Theme) => theme.vars.shape.radiusPill }}
      />
    );
  }
  return (
    <GradientIconWrapper size="2.25rem">
      <Box
        component={isMcpToolkitType(toolkit.type) ? McpIcon : ToolIcon}
        sx={iconGlyphSx}
      />
    </GradientIconWrapper>
  );
}

interface ToolkitMentionItemProps {
  readonly toolkit: SlashParticipantToolkit;
  readonly onClick: () => void;
  readonly isActive: boolean;
}

function ToolkitMentionItem({ toolkit, onClick, isActive }: ToolkitMentionItemProps): ReactNode {
  return (
    <ButtonBase
      onClick={onClick}
      disableRipple
      data-highlighted={isActive ? 'true' : undefined}
      sx={(theme: Theme) => ({
        display: 'flex',
        alignItems: 'center',
        gap: '0.75rem',
        width: '100%',
        textAlign: 'left',
        padding: '0.5rem 0.75rem',
        borderRadius: theme.vars.shape.radiusMd,
        cursor: 'pointer',
        background: isActive ? theme.vars.palette.background.userInputBackgroundActive : theme.vars.palette.background.userInputBackground,
        '&:hover': { background: theme.vars.palette.background.userInputBackgroundActive },
      })}
    >
      <ToolkitMentionIcon toolkit={toolkit} />
      <Box sx={{ minWidth: 0, flex: 1 }}>
        <Typography
          variant="headingSmall"
          color="text.secondary"
          sx={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
        >
          {toolkit.name}
        </Typography>
        <Typography
          variant="bodySmall"
          color="text.default"
          sx={{ textTransform: 'capitalize', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
        >
          {isMcpToolkitType(toolkit.type) ? 'MCP' : t('chatInput.toolkitMentionList.toolkit', 'Toolkit')}
        </Typography>
      </Box>
    </ButtonBase>
  );
}

/** The list's first row, `Skills` (ADR-0050 amendment 1, A1.5): a pick hands over to the skill picker. */
function SkillsEntry({ onClick, isActive }: { readonly onClick: () => void; readonly isActive: boolean }): ReactNode {
  return (
    <ButtonBase
      onClick={onClick}
      disableRipple
      data-testid="slash-skills-entry"
      data-highlighted={isActive ? 'true' : undefined}
      sx={(theme: Theme) => ({
        display: 'flex',
        alignItems: 'center',
        gap: '0.75rem',
        width: '100%',
        textAlign: 'left',
        padding: '0.5rem 0.75rem',
        borderRadius: theme.vars.shape.radiusMd,
        cursor: 'pointer',
        background: isActive ? theme.vars.palette.background.userInputBackgroundActive : theme.vars.palette.background.userInputBackground,
        '&:hover': { background: theme.vars.palette.background.userInputBackgroundActive },
      })}
    >
      <GradientIconWrapper size="2.25rem">
        <Box
          component={SkillIcon}
          sx={iconGlyphSx}
        />
      </GradientIconWrapper>
      <Box sx={{ minWidth: 0, flex: 1 }}>
        <Typography
          variant="headingSmall"
          color="text.secondary"
        >
          {t('chatInput.toolkitMentionList.skills', 'Skills')}
        </Typography>
        <Typography
          variant="bodySmall"
          color="text.default"
        >
          {t('chatInput.toolkitMentionList.skillsHint', 'Add a skill to the next message')}
        </Typography>
      </Box>
    </ButtonBase>
  );
}

export interface ToolkitMentionListProps {
  /** Shows the first `Skills` row; a pick calls it. Absent when no skill can be mentioned. */
  readonly onSelectSkills?: (() => void) | undefined;
  readonly toolkits: readonly SlashParticipantToolkit[];
  readonly onSelectToolkit: (toolkit: SlashParticipantToolkit) => void;
  readonly onClose: () => void;
  readonly title: string;
  readonly activeIndex: number;
  readonly 'data-testid'?: string;
}

export function ToolkitMentionList({ onSelectSkills, toolkits, onSelectToolkit, onClose, title, activeIndex, 'data-testid': dataTestId }: ToolkitMentionListProps): ReactNode {
  const containerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const container = containerRef.current;
    if (!container || activeIndex < 0) return;
    const highlighted = container.querySelector('[data-highlighted="true"]');
    if (!highlighted) return;
    const stickyHeader = container.firstElementChild;
    const headerHeight = stickyHeader instanceof HTMLElement ? stickyHeader.offsetHeight : 0;
    const containerRect = container.getBoundingClientRect();
    const itemRect = highlighted.getBoundingClientRect();
    const itemTopRelative = itemRect.top - containerRect.top;
    const itemBottomRelative = itemRect.bottom - containerRect.top;
    if (itemTopRelative < headerHeight) {
      container.scrollTop += itemTopRelative - headerHeight;
    } else if (itemBottomRelative > container.clientHeight) {
      container.scrollTop += itemBottomRelative - container.clientHeight;
    }
  }, [activeIndex]);

  return (
    <ClickAwayListener onClickAway={onClose}>
      <Box
        ref={containerRef}
        data-testid={dataTestId}
        sx={(theme: Theme) => ({
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
          width: '100%',
          maxWidth: '100%',
          maxHeight: '15.4375rem',
          borderRadius: theme.vars.shape.radiusLg,
          boxSizing: 'border-box',
          padding: '0.75rem',
          display: 'flex',
          flexDirection: 'column',
          gap: '0.5rem',
          background: theme.vars.palette.background.secondary,
          overflowY: 'auto',
        })}
      >
        <Box
          sx={{
            position: 'sticky',
            top: '-0.75rem',
            zIndex: 1,
            height: '1rem',
            display: 'flex',
            alignItems: 'center',
            padding: '1rem 0.75rem',
            margin: '-0.75rem -0.75rem 0',
            background: 'inherit',
          }}
        >
          <Typography
            variant="subtitle"
            color="text.primary"
          >
            {title}
          </Typography>
        </Box>

        {onSelectSkills !== undefined && (
          <SkillsEntry
            onClick={onSelectSkills}
            isActive={activeIndex === 0}
          />
        )}
        {toolkits.length === 0 && onSelectSkills === undefined ? (
          <Typography
            variant="bodyMedium"
            color="text.secondary"
            sx={{ padding: '0 0.5rem' }}
          >
            {t('chatInput.toolkitMentionList.noResults', 'No matching results')}
          </Typography>
        ) : (
          toolkits.map((toolkit, index) => (
            <ToolkitMentionItem
              key={`${toolkit.projectId}_${toolkit.id}`}
              toolkit={toolkit}
              onClick={() => onSelectToolkit(toolkit)}
              isActive={index + (onSelectSkills === undefined ? 0 : 1) === activeIndex}
            />
          ))
        )}
      </Box>
    </ClickAwayListener>
  );
}
