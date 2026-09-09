/**
 * The run-history panel behind agents', pipelines' and toolkits'
 * `renderRunHistory`-shaped slot (issue #868) — one component shared by all
 * three editors, since the underlying data is the same shape regardless of
 * which entity's page opened it: conversations filtered by `entity_name` +
 * `entity_meta_id` (`GET /elitea_core/conversations/prompt_lib/{projectId}`,
 * newly documented in v2.yaml/the generated client — the Go route itself
 * already supported this filter, see `internal/api/v2/conversations/
 * handler.go`'s `List`).
 *
 * `onRestoreConversation`, when supplied, adds a "Restore" action per row.
 * It is omitted where the caller's editor has no live chat pane to restore
 * into — toolkits' `ConfigurationTab` run-history slot carries no such
 * callback at all, and pipelines' does but nothing downstream of
 * `usePipelineChat` currently consumes a restored conversation id either
 * (the SAME disclosed, partially-ported state `features/agents/ui/
 * ConfigurationTab.tsx`'s own doc comment records for its identical
 * `onRestoreConversation` prop). The prop is still threaded through
 * faithfully — a caller wiring a real restore path later needs no change
 * here.
 */
import { useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';
import CloseIcon from '@mui/icons-material/Close';

import { useListConversations } from '@/shared/api/generated/chat/chat';
import type { ConversationSummary } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';

import { RunHistoryList } from './RunHistoryList';
import { RunHistoryTrace } from './RunHistoryTrace';

/** `entity_name` the run-history filter reads — see `listConversations`'s own description in v2.yaml. */
export type RunHistoryEntityName = 'application' | 'toolkit';

export interface RunHistoryPanelProps {
  readonly projectId: string | undefined;
  readonly entityName: RunHistoryEntityName;
  readonly entityId: string | number | undefined;
  readonly onClose: () => void;
  readonly onRestoreConversation?: (conversationId: string) => void;
}

const rootSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem', height: '100%', width: '100%' };
const headerSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'space-between' };
const bodySx: SxProps<Theme> = { display: 'flex', gap: '1.5rem', flex: 1, minHeight: 0 };
const listPaneSx: SxProps<Theme> = { flex: 1, minWidth: 0, overflowY: 'auto' };
const tracePaneSx: SxProps<Theme> = { flex: 1, minWidth: 0, overflowY: 'auto' };
const loadingSx: SxProps<Theme> = { display: 'flex', justifyContent: 'center', padding: '2rem' };

interface RunHistoryPanelBodyProps {
  readonly isLoading: boolean;
  readonly rows: readonly ConversationSummary[];
  readonly selected: ConversationSummary | undefined;
  readonly onSelect: (row: ConversationSummary) => void;
  readonly projectId: string | undefined;
  readonly onRestoreConversation?: ((conversationId: string) => void) | undefined;
}

/** The list-or-trace body — split out of `RunHistoryPanel` purely to keep that function's cyclomatic complexity under this codebase's gate (12). */
function RunHistoryPanelBody({
  isLoading,
  rows,
  selected,
  onSelect,
  projectId,
  onRestoreConversation,
}: RunHistoryPanelBodyProps): ReactNode {
  if (isLoading) {
    return (
      <Box sx={loadingSx} data-testid="run-history-loading">
        <CircularProgress size={28} />
      </Box>
    );
  }

  if (rows.length === 0) {
    return (
      <NoResultsMessage
        title={t('entities.runHistory.emptyTitle', 'No runs yet')}
        description={t(
          'entities.runHistory.empty',
          'This has not been used in a conversation yet. Attach it to a chat to start its history.',
        )}
      />
    );
  }

  return (
    <Box sx={bodySx}>
      <Box sx={listPaneSx}>
        <RunHistoryList
          rows={rows}
          selectedId={selected?.id}
          onSelect={onSelect}
          {...(onRestoreConversation !== undefined ? { onRestoreConversation } : {})}
        />
      </Box>
      <Box sx={tracePaneSx}>
        {selected !== undefined && projectId !== undefined ? (
          <RunHistoryTrace projectId={projectId} conversationId={String(selected.id)} />
        ) : (
          <Typography variant="bodyMedium" color="text.secondary" data-testid="run-history-no-selection">
            {t('entities.runHistory.selectPrompt', 'Select a run to see its trace.')}
          </Typography>
        )}
      </Box>
    </Box>
  );
}

export function RunHistoryPanel({
  projectId,
  entityName,
  entityId,
  onClose,
  onRestoreConversation,
}: RunHistoryPanelProps): ReactNode {
  const [selected, setSelected] = useState<ConversationSummary | undefined>(undefined);

  const enabled = projectId !== undefined && entityId !== undefined;
  const listQuery = useListConversations(
    projectId ?? '',
    { entity_name: entityName, entity_meta_id: String(entityId ?? ''), limit: 50 },
    { query: { enabled } },
  );
  // `.data.data`'s declared type includes the error-envelope variant — never
  // actually reachable here since `eliteaFetch` throws instead of resolving
  // with it (mutator.ts's §3.6 unwrap contract) — same cast PrivateAgentsList
  // already establishes for a generated list hook.
  const listing = listQuery.data?.data as { readonly rows: readonly ConversationSummary[] } | undefined;
  const rows = listing?.rows ?? [];

  return (
    <Box sx={rootSx} data-testid="run-history-panel">
      <Box sx={headerSx}>
        <Typography variant="headingSmall">{t('entities.runHistory.title', 'Run history')}</Typography>
        <IconButton
          data-testid="run-history-close"
          aria-label={t('entities.runHistory.close', 'Close run history')}
          onClick={onClose}
        >
          <CloseIcon fontSize="small" />
        </IconButton>
      </Box>

      <RunHistoryPanelBody
        isLoading={!enabled || listQuery.isPending}
        rows={rows}
        selected={selected}
        onSelect={setSelected}
        projectId={projectId}
        {...(onRestoreConversation !== undefined ? { onRestoreConversation } : {})}
      />
    </Box>
  );
}
