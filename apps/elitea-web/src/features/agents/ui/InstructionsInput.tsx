import type { ReactNode, RefObject } from 'react';
import { useCallback, useEffect, useMemo, useRef } from 'react';

import type { Extension } from '@codemirror/state';
import { EditorView, ViewPlugin } from '@codemirror/view';
import Box from '@mui/material/Box';
import ClickAwayListener from '@mui/material/ClickAwayListener';
import type { SxProps, Theme } from '@mui/material/styles';
import { useTheme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { CodeMirrorEditor } from '@/shared/ui/CodeMirrorEditor';
import { MentionToolItem } from '@/shared/ui/MentionToolItem';

import { MentionPhase, type MentionPhaseValue } from '../lib/constants/mention.constants';
import type { KeyDownEvent } from '../lib/hooks/instructionsMention.helpers';
import { useInstructionsMention } from '../lib/hooks/useInstructionsMention.hooks';
import type { FileReaderInputHandle, MentionableTool } from '../lib/hooks/useInstructionsMention.hooks';
import { useInstructionsSkillMention } from '../lib/hooks/useInstructionsSkillMention.hooks';
import type { FilteredSkillMentionItem } from '../lib/hooks/useInstructionsSkillMention.hooks';

import { InstructionsFullscreenButton } from './InstructionsFullscreenButton';
import { InstructionsSlashSuggestionList } from './InstructionsSlashSuggestionList';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent/ui/agent-details/
 * configurations/input/InstructionsInput.jsx`.
 *
 * DISCLOSED REDESIGN, forced by a real `shared/ui` constraint: the
 * baseline's mirror-overlay-sync machinery needs `FileReaderEnhancer`'s
 * ref-based imperative handle, which this app's `FileReaderInput.tsx`
 * structurally does not expose. `shared/ui/CodeMirrorEditor` (unit S1-E) is
 * used instead.
 *
 * **CodeMirror <-> mention-hook bridge (this file's own addition — NOT a
 * `shared/ui/CodeMirrorEditor` change):** `useInstructionsMention`/
 * `useInstructionsSkillMention` need a `fileReaderRef:
 * RefObject<FileReaderInputHandle | null>` (`getCursorPosition`/
 * `getInputContent`/`replaceRange`) and an `onKeyDown`. `CodeMirrorEditor`
 * (out of this cluster's scope) exposes neither, so this file adapts
 * entirely through its one real extension point, `extensions:
 * Extension[]` — CM6's own mechanism, no `CodeMirrorEditor` change needed:
 *  - a `ViewPlugin` captures the live `EditorView` once on mount, backing
 *    the ref's three methods against `EditorState` directly
 *    (`selection.main.head`/`doc.toString()`/`view.dispatch({changes})`).
 *  - `EditorView.domEventHandlers({ keydown })` runs before CM6's own
 *    keymap (a handler returning `true` suppresses all further/built-in
 *    handling, per `@codemirror/view`'s own contract), so ArrowUp/
 *    ArrowDown/Enter can be captured for the active dropdown and fully
 *    prevented from moving the cursor, while the "/"/"~" triggers
 *    themselves (never `preventDefault()`-ed by the hooks) still reach
 *    CM6's normal typing path. `useInstructionsSlashCommand.hooks.ts`'s own
 *    `onKeyDown` doc comment already anticipated this exact wiring: "For
 *    CodeMirror the [`selectionStart`] property is absent — anchor is set
 *    later in `syncWithValue`" — `target` is deliberately omitted from the
 *    wrapped event for that reason.
 *
 * **Still not reproduced:** file-drop / "attach a file" — `CodeMirrorEditor`
 * has no file-drop affordance and `FileReaderInput` has one but no
 * extension slot; this component keeps the extension slot (mentions) over
 * file-drop, same trade-off the pre-existing port already made.
 *
 * **Real, disclosed gap OUTSIDE this cluster's scope:** this component is
 * now fully wired and independently tested (see the sibling `.test.tsx`),
 * but its sole caller, `CreateAgentForm.tsx` (a sibling A1 unit), does not
 * yet pass `tools`/`versionId` through — without them the picker never has
 * anything to show. `CreateAgentForm.tsx` needs
 * `tools={versionDetails?.tools} versionId={versionDetails?.id}` added to
 * its existing `<InstructionsInput>` call.
 *
 * `mentionableItems` (a pre-computed, passive-highlight-only prop) is
 * replaced by `tools`/`versionId`/`isToolLoggedIn`, matching the baseline's
 * actual data source and deriving one item list for both the picker and the
 * highlight decorations, instead of requiring the caller to pre-filter it.
 *
 * `maxLength={MAX_INSTRUCTIONS_LENGTH}` (a Skills-only constant,
 * `shared/lib/limits.ts`) is NOT applied here — the baseline's agent
 * Instructions field has no length limit at all; applying it silently
 * truncated any existing agent's instructions past 2500 characters on the
 * very next edit.
 */
export interface InstructionsInputProps {
  readonly instructions: string | undefined;
  readonly onInstructionsChange: (value: string) => void;
  readonly disabled?: boolean | undefined;
  readonly applicationId?: number | string | undefined;
  readonly entityProjectId?: string | undefined;
  /** The version whose `tools[]` populate the "/" mention list and gate the "~" skill list — `version_details.id` at the caller. */
  readonly versionId?: string | number | undefined;
  /** `version_details.tools` at the caller — drives both the "/" mention picker and its highlight decorations. */
  readonly tools?: readonly MentionableTool[] | undefined;
  /** See `useInstructionsMention`'s own doc comment, deviation 2 — optional injected MCP-login-state predicate. Omit to treat every MCP tool as logged in (no additional filtering). */
  readonly isToolLoggedIn?: ((tool: MentionableTool) => boolean) | undefined;
}

/** Presentational "~" skill-mention dropdown — ported inline from the baseline's `features/skill/ui/MentionSkillList.jsx` (no `features/skill` slice exists in this app yet to host a standalone port; this cluster owns `InstructionsInput.tsx`, not a new file). Structurally identical to `InstructionsSlashSuggestionList`'s own "items" branch, minus the tools drill-down phase skills don't have. */
interface InstructionsSkillMentionListProps {
  readonly phase: MentionPhaseValue;
  readonly filteredItems: readonly FilteredSkillMentionItem[];
  readonly highlightedIndex: number;
  readonly onSelectItem: (item: FilteredSkillMentionItem) => void;
  readonly onClose: () => void;
}

function InstructionsSkillMentionList({
  phase,
  filteredItems,
  highlightedIndex,
  onSelectItem,
  onClose,
}: InstructionsSkillMentionListProps): ReactNode {
  const containerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!containerRef.current || highlightedIndex < 0) return;
    const container = containerRef.current;
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

  if (phase === MentionPhase.Idle) return null;

  return (
    <ClickAwayListener onClickAway={onClose}>
      <Box
        ref={containerRef}
        sx={skillListContainerSx}
      >
        <Box sx={skillListHeaderSx}>
          <Typography
            variant="subtitle"
            color="text.primary"
          >
            {t('features.agents.instructionsSkillMentionList.header', 'Mention skill')}
          </Typography>
        </Box>
        {filteredItems.length === 0 ? (
          <Box sx={skillListEmptySx}>
            <Typography
              variant="bodySmall"
              color="text.secondary"
            >
              {t('features.agents.instructionsSkillMentionList.empty', 'No skills attached to this agent')}
            </Typography>
          </Box>
        ) : (
          filteredItems.map((item, index) => (
            <MentionToolItem
              key={item.name}
              label={item.name}
              {...(item.description !== undefined ? { description: item.description } : {})}
              onClick={() => onSelectItem(item)}
              isHighlighted={index === highlightedIndex}
            />
          ))
        )}
      </Box>
    </ClickAwayListener>
  );
}

export function InstructionsInput({
  instructions,
  onInstructionsChange,
  disabled,
  applicationId,
  entityProjectId,
  versionId,
  tools,
  isToolLoggedIn,
}: InstructionsInputProps): ReactNode {
  const theme = useTheme();
  const highlightColor = theme.vars.palette.text.info;

  // ── CodeMirror <-> mention-hook bridge — see module doc comment. ───────────────
  const viewRef = useRef<EditorView | null>(null);
  const fileReaderRef: RefObject<FileReaderInputHandle | null> = useRef<FileReaderInputHandle | null>({
    getCursorPosition: () => viewRef.current?.state.selection.main.head ?? 0,
    getInputContent: () => viewRef.current?.state.doc.toString() ?? '',
    replaceRange: (start, end, replacement) => {
      viewRef.current?.dispatch({ changes: { from: start, to: end, insert: replacement } });
    },
  });

  const viewCaptureExtension = useMemo<Extension>(
    () =>
      ViewPlugin.define((view) => {
        viewRef.current = view;
        return {
          destroy: () => {
            if (viewRef.current === view) viewRef.current = null;
          },
        };
      }),
    [],
  );

  const mention = useInstructionsMention({
    fileReaderRef,
    applicationId,
    projectId: entityProjectId,
    versionId,
    tools,
    instructions,
    highlightColor,
    ...(isToolLoggedIn !== undefined ? { isToolLoggedIn } : {}),
  });

  const skillMention = useInstructionsSkillMention({
    fileReaderRef,
    projectId: entityProjectId,
    versionId,
    instructions,
    highlightColor,
  });

  const isSkillPhaseActive = skillMention.phase !== MentionPhase.Idle;
  const isSlashPhaseActive = mention.phase !== MentionPhase.Idle;

  // Route keydown to the active machine; when both idle, let both see the keypress so each can
  // detect ONLY its own trigger ("/" vs "~"). Byte-for-byte port of the baseline's own
  // `combinedOnKeyDown` (`InstructionsInput.jsx:160-174`).
  const combinedOnKeyDown = useCallback(
    (event: KeyDownEvent) => {
      if (isSkillPhaseActive) {
        skillMention.onKeyDown(event);
        return;
      }
      if (isSlashPhaseActive) {
        mention.onKeyDown(event);
        return;
      }
      mention.onKeyDown(event);
      skillMention.onKeyDown(event);
    },
    [isSkillPhaseActive, isSlashPhaseActive, mention, skillMention],
  );

  // "Latest ref" so `keydownExtension` (below) keeps a stable identity across renders instead of
  // forcing a CodeMirror reconfigure on every keystroke.
  const onKeyDownRef = useRef(combinedOnKeyDown);
  onKeyDownRef.current = combinedOnKeyDown;

  const keydownExtension = useMemo<Extension>(
    () =>
      EditorView.domEventHandlers({
        keydown: (event) => {
          let handled = false;
          onKeyDownRef.current({
            key: event.key,
            preventDefault: () => {
              handled = true;
              event.preventDefault();
            },
          });
          return handled;
        },
      }),
    [],
  );

  const handleChange = useCallback(
    (value: string) => {
      onInstructionsChange(value);
      mention.onInstructionsInputChange(value);
      skillMention.onInstructionsInputChange(value);
    },
    [onInstructionsChange, mention, skillMention],
  );

  const extensions = useMemo(
    () => [...mention.codeMirrorExtensions, ...skillMention.codeMirrorExtensions, viewCaptureExtension, keydownExtension],
    [mention.codeMirrorExtensions, skillMention.codeMirrorExtensions, viewCaptureExtension, keydownExtension],
  );

  // `InstructionsSlashSuggestionList.onSelectItem` is typed against its own narrow
  // presentational item shape; the hook's `onSelectItem` needs the full `FilteredMentionableItem`
  // (settings/type/etc, for `computeToolReplacement`) — recovered here by name from the same
  // `filteredItems` array the list was just rendered from.
  const handleSelectSlashItem = useCallback(
    (item: { readonly name: string }, isToolkit: boolean) => {
      const match = mention.filteredItems.find((candidate) => candidate.name === item.name);
      if (match) mention.onSelectItem(match, isToolkit);
    },
    [mention],
  );

  const activeSuggestionList = isSkillPhaseActive ? (
    <InstructionsSkillMentionList
      phase={skillMention.phase}
      filteredItems={skillMention.filteredItems}
      highlightedIndex={skillMention.highlightedIndex}
      onSelectItem={skillMention.onSelectItem}
      onClose={skillMention.resetSlash}
    />
  ) : (
    <InstructionsSlashSuggestionList
      phase={mention.phase}
      filteredItems={mention.filteredItems}
      filteredTools={mention.filteredTools}
      selectedItem={mention.selectedItem ?? undefined}
      highlightedIndex={mention.highlightedIndex}
      onSelectItem={handleSelectSlashItem}
      onSelectTool={mention.onSelectTool}
      onClose={mention.resetSlash}
    />
  );

  // Not `useMemo`'d: its `content` unavoidably closes over `activeSuggestionList`, a fresh JSX
  // element every render (it can never be referentially stable), so memoizing this array would
  // recompute on every render anyway — same as not memoizing, minus the dependency-array upkeep.
  const accordionItems = [
    {
      title: 'Instructions',
      content: (
        <Box sx={wrapperSx}>
          <InstructionsFullscreenButton value={instructions ?? ''} onChange={handleChange} disabled={disabled} />
          <CodeMirrorEditor
            value={instructions ?? ''}
            onChange={handleChange}
            extensions={extensions}
            readOnly={disabled}
            minHeight="8rem"
            aria-label={t('features.agents.instructionsInput.ariaLabel', 'Instructions')}
          />
          {activeSuggestionList}
        </Box>
      ),
    },
  ];

  return (
    <BasicAccordion
      showMode="left"
      slotSx={{ accordion: accordionSx }}
      items={accordionItems}
    />
  );
}

const accordionSx: SxProps<Theme> = (theme: Theme) => ({
  background: theme.vars.palette.background.tabPanel,
});

const wrapperSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
};

const skillListContainerSx: SxProps<Theme> = (theme: Theme) => ({
  border: `1px solid ${theme.vars.palette.border.lines}`,
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
});

const skillListHeaderSx: SxProps<Theme> = {
  position: 'sticky',
  top: '-0.75rem',
  zIndex: 1,
  height: '1rem',
  display: 'flex',
  alignItems: 'center',
  padding: '1rem 0.75rem',
  margin: '-0.75rem -0.75rem 0',
  background: 'inherit',
};

const skillListEmptySx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  padding: '0.5rem 0.75rem',
};
