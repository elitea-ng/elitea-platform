/**
 * The question box, the capability toggle and the two controls that act on a
 * finished turn.
 *
 * ENTER SENDS, Shift+Enter breaks the line. That is what the legacy drawer did
 * and what every chat surface in this app does; a question about code is often
 * several lines, so the modifier matters.
 */
import { memo, useCallback, useState, type KeyboardEvent } from 'react';

import IconButton from '@mui/material/IconButton';
import SendIcon from '@mui/icons-material/Send';
import RefreshIcon from '@mui/icons-material/Refresh';
import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import Stack from '@mui/material/Stack';
import TextField from '@mui/material/TextField';
import ToggleButton from '@mui/material/ToggleButton';
import ToggleButtonGroup from '@mui/material/ToggleButtonGroup';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';
import type { ChatCapability } from '@/features/wiki-chat';

import { WikiContextPicker } from './WikiContextPicker';

export interface WikiChatComposerProps {
  readonly mode: ChatCapability;
  readonly onModeChange: (mode: ChatCapability) => void;
  readonly onSend: (question: string) => void;
  readonly onRegenerate: () => void;
  readonly onClear: () => void;
  readonly isLoading: boolean;
  readonly canRegenerate: boolean;
  /**
   * The open wiki version's page ids, offered as attachable context. Empty
   * (or a wiki with no pages) hides the control entirely rather than
   * offering a picker with nothing in it.
   */
  readonly contextPages: readonly string[];
  /** The ids attached to the NEXT question, in selection order. */
  readonly contextPaths: readonly string[];
  readonly onContextPathsChange: (selected: readonly string[]) => void;
}

export const WikiChatComposer = memo(function WikiChatComposer({
  mode,
  onModeChange,
  onSend,
  onRegenerate,
  onClear,
  isLoading,
  canRegenerate,
  contextPages,
  contextPaths,
  onContextPathsChange,
}: WikiChatComposerProps) {
  const [question, setQuestion] = useState('');

  const send = useCallback(() => {
    if (question.trim() === '' || isLoading) return;
    onSend(question);
    // Cleared HERE rather than by the hook: the hook owns the conversation, and
    // the draft is this component's. Clearing it from outside would wipe what
    // the user typed while a previous answer was still arriving.
    setQuestion('');
  }, [isLoading, onSend, question]);

  const onKeyDown = useCallback(
    (event: KeyboardEvent<HTMLDivElement>) => {
      if (event.key === 'Enter' && !event.shiftKey) {
        event.preventDefault();
        send();
      }
    },
    [send],
  );

  return (
    <Stack sx={{ gap: 1, p: 1.5, borderTop: 1, borderColor: 'divider' }}>
      <Stack sx={{ flexDirection: 'row', gap: 1, alignItems: 'center' }}>
        <ToggleButtonGroup
          exclusive
          size="small"
          value={mode}
          aria-label={t('widgets.deepwiki.chat.modeLabel', 'Answer mode')}
          onChange={(_event, next: ChatCapability | null) => {
            // MUI reports a deselect as null. Ignoring it keeps the group from
            // reaching a state with neither button chosen, which would send the
            // next question with no capability at all.
            if (next) onModeChange(next);
          }}
        >
          <ToggleButton value="ask">{t('widgets.deepwiki.chat.modeAsk', 'Ask')}</ToggleButton>
          <ToggleButton value="research">
            {t('widgets.deepwiki.chat.modeResearch', 'Research')}
          </ToggleButton>
        </ToggleButtonGroup>

        <WikiContextPicker
          pages={contextPages}
          selected={contextPaths}
          onChange={onContextPathsChange}
          disabled={isLoading}
        />

        <Stack sx={{ flexDirection: 'row', gap: 0.5, marginLeft: 'auto' }}>
          <Tooltip title={t('widgets.deepwiki.chat.regenerate', 'Ask again')}>
            {/* A disabled button cannot fire the tooltip's own events, so the
                span is what keeps the explanation reachable while it is off. */}
            <span>
              <IconButton
                size="small"
                onClick={onRegenerate}
                disabled={isLoading || !canRegenerate}
                aria-label={t('widgets.deepwiki.chat.regenerate', 'Ask again')}
              >
                <RefreshIcon fontSize="small" />
              </IconButton>
            </span>
          </Tooltip>
          <Tooltip title={t('widgets.deepwiki.chat.clear', 'Clear the conversation')}>
            <span>
              <IconButton
                size="small"
                onClick={onClear}
                disabled={isLoading}
                aria-label={t('widgets.deepwiki.chat.clear', 'Clear the conversation')}
              >
                <DeleteOutlinedIcon fontSize="small" />
              </IconButton>
            </span>
          </Tooltip>
        </Stack>
      </Stack>

      <Stack sx={{ flexDirection: 'row', gap: 1, alignItems: 'flex-end' }}>
        <TextField
          fullWidth
          multiline
          maxRows={6}
          size="small"
          value={question}
          onChange={(event) => {
            setQuestion(event.target.value);
          }}
          onKeyDown={onKeyDown}
          placeholder={t('widgets.deepwiki.chat.placeholder', 'Ask about this repository')}
          label={t('widgets.deepwiki.chat.questionLabel', 'Question')}
        />
        <IconButton
          color="primary"
          onClick={send}
          disabled={isLoading || question.trim() === ''}
          aria-label={t('widgets.deepwiki.chat.send', 'Send')}
        >
          <SendIcon />
        </IconButton>
      </Stack>
    </Stack>
  );
});
