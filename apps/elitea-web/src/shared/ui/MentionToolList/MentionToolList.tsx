import type { ReactNode } from 'react';
import { Fragment, useEffect, useRef } from 'react';

import Box from '@mui/material/Box';
import ClickAwayListener from '@mui/material/ClickAwayListener';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { MentionToolItem } from '../MentionToolItem';

/** @public */
export interface MentionTool {
  name: string;
  description?: string;
  /** A section title: shown above the first row of each run of rows with the same group. */
  group?: string;
}

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface MentionToolListProps {
  tools: MentionTool[];
  toolkitName: string;
  onSelectTool: (toolName: string | null) => void;
  highlightedIndex: number;
  'data-testid'?: string;
}

/**
 * The `@`-mention autocomplete popup: a sticky toolkit-name header over a
 * scrollable list of `MentionToolItem` rows, closing on an outside click.
 * Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/mention/MentionToolList.jsx`.
 * `onSelectTool(null)` is the baseline's own "close without picking a
 * tool" signal (fired by `ClickAwayListener`), kept as-is rather than
 * adding a second `onClose` callback for the same 12-prop budget reason
 * `BaseModal` groups its extras into option objects — one callback,
 * `null` means dismiss.
 */
export function MentionToolList({
  tools,
  toolkitName,
  onSelectTool,
  highlightedIndex,
  'data-testid': dataTestId,
}: MentionToolListProps): ReactNode {
  const containerRef = useRef<HTMLDivElement>(null);

  // Scroll the highlighted item into view, accounting for the sticky header.
  useEffect(() => {
    const container = containerRef.current;
    if (!container || highlightedIndex < 0) return;
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
  }, [highlightedIndex]);

  return (
    <ClickAwayListener onClickAway={() => onSelectTool(null)}>
      <Box
        ref={containerRef}
        data-testid={dataTestId}
        sx={(theme) => ({
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
            {toolkitName} {t('shared.ui.mentionToolList.availableTools', 'available tools')}
          </Typography>
        </Box>

        {tools.map((tool, index) => (
          <Fragment key={tool.name}>
            {tool.group !== undefined && tool.group !== tools[index - 1]?.group && (
              <Typography variant="labelSmall" color="text.secondary" sx={{ padding: '0.25rem 0.75rem 0' }}>
                {tool.group}
              </Typography>
            )}
            <MentionToolItem
              label={tool.name}
              {...(tool.description !== undefined ? { description: tool.description } : {})}
              onClick={() => onSelectTool(tool.name)}
              isHighlighted={index === highlightedIndex}
            />
          </Fragment>
        ))}
      </Box>
    </ClickAwayListener>
  );
}
