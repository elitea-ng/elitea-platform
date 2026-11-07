/**
 * The agent editor's Memory tab (ADR-0050 decision 1b, issue 1608): what the
 * agent remembers in this project.
 *
 * Search and filter the entries; approve or reject what waits for approval;
 * edit, pin, archive or delete an entry; read its history; export what the
 * filters match as JSON; clear everything; and turn off "remember things
 * about me". The server decides what this caller may see and do (an entry
 * about another person is listed only for that person and the agent's
 * managers), and the list says so in `can_manage`, `can_delete` and
 * `can_approve`; the tab offers nothing more than that.
 *
 * The desktop app runs this same web build, so the tab is there too.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import FormControlLabel from '@mui/material/FormControlLabel';
import Snackbar from '@mui/material/Snackbar';
import Switch from '@mui/material/Switch';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { MEMORY_PAGE_SIZE } from '../lib/memoryFilters';
import { useMemoryPanel } from '../model/useMemoryPanel';

type MemoryPanelModel = ReturnType<typeof useMemoryPanel>;
import { MemoryEditDialog } from './MemoryEditDialog';
import { MemoryEntryRow } from './MemoryEntryRow';
import { MemoryFiltersBar } from './MemoryFiltersBar';
import { MemoryHistoryDialog } from './MemoryHistoryDialog';

export interface MemoryPanelProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
}

export function MemoryPanel({ projectId, applicationId }: MemoryPanelProps): ReactNode {
  if (projectId === undefined || projectId === '' || applicationId === undefined || applicationId <= 0) {
    return (
      <Typography variant="bodyMedium" color="text.secondary" data-testid="agent-memory-unsaved">
        {t('features.agentMemory.unsaved', 'Save the agent first: its memory starts with its first conversation.')}
      </Typography>
    );
  }
  return <MemoryPanelBody projectId={projectId} applicationId={applicationId} />;
}

function MemoryPanelBody({ projectId, applicationId }: { readonly projectId: string; readonly applicationId: number }): ReactNode {
  const panel = useMemoryPanel(projectId, applicationId);
  const { list, rows, dialogs, page } = panel;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }} data-testid="agent-memory-panel">
      <Typography variant="bodyMedium" color="text.secondary">
        {t(
          'features.agentMemory.description',
          'Notes this agent keeps across conversations in this project. A note about a person is shown only to that person and to the agent’s managers.',
        )}
      </Typography>
      <MemoryToolbar panel={panel} />
      {panel.optedOut && (
        <Alert severity="info" data-testid="agent-memory-opted-out">
          {t('features.agentMemory.optedOut', 'This agent does not remember anything about you. Turn the switch back on to let it.')}
        </Alert>
      )}
      <MemoryFiltersBar filters={panel.filters} people={[...panel.names].map(([id, name]) => ({ id, name }))} onChange={panel.setFilters} />
      {list.isLoading && <CircularProgress size="1.5rem" />}
      {list.isError && (
        <Alert severity="error" data-testid="agent-memory-error">
          {t('features.agentMemory.error.load', 'The agent’s memory could not be loaded.')}
        </Alert>
      )}
      {list.isSuccess && rows.length === 0 && (
        <Typography variant="bodyMedium" color="text.secondary" data-testid="agent-memory-empty">
          {t('features.agentMemory.empty', 'Nothing remembered yet, or nothing matches these filters.')}
        </Typography>
      )}
      <Box>
        {rows.map((entry) => (
          <MemoryEntryRow key={entry.id} entry={entry} names={panel.names} abilities={panel.abilities} busy={panel.busy} onAction={panel.onAction} />
        ))}
      </Box>
      <MemoryPager page={page} total={panel.total} />
      <MemoryEditDialog
        projectId={projectId}
        applicationId={applicationId}
        entry={dialogs.editing}
        canChangeKind={panel.abilities.canManage}
        onClose={dialogs.closeEdit}
      />
      <MemoryHistoryDialog projectId={projectId} applicationId={applicationId} entryId={dialogs.historyOf} names={panel.names} onClose={dialogs.closeHistory} />
      <DeleteEntityModal
        open={dialogs.deleting !== undefined}
        onClose={dialogs.closeDelete}
        onConfirm={dialogs.confirmDelete}
        copy={{ title: t('features.agentMemory.deleteTitle', 'Delete this memory?') }}
        data-testid="agent-memory-delete-dialog"
      />
      <DeleteEntityModal
        open={dialogs.confirmClear}
        onClose={dialogs.closeClear}
        onConfirm={dialogs.runClear}
        copy={{
          title: t('features.agentMemory.clearTitle', 'Clear all of this agent’s memory?'),
          textContent: t('features.agentMemory.clearText', 'Every note the agent keeps in this project is deleted, for everyone. This cannot be undone.'),
        }}
        data-testid="agent-memory-clear-dialog"
      />
      <Snackbar open={panel.notice !== undefined} autoHideDuration={6000} onClose={panel.closeNotice}>
        <Alert severity={panel.notice?.severity ?? 'success'} onClose={panel.closeNotice} data-testid="agent-memory-notice">
          {panel.notice?.message}
        </Alert>
      </Snackbar>
    </Box>
  );
}

function MemoryToolbar({ panel }: { readonly panel: MemoryPanelModel }): ReactNode {
  const { list, dialogs } = panel;
  return (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: '0.75rem', alignItems: 'center', justifyContent: 'space-between' }}>
      <FormControlLabel
        control={
          <Switch
            checked={!panel.optedOut}
            disabled={list.data === undefined || panel.optOutPending}
            onChange={(_event, checked) => panel.toggleOptOut(!checked)}
            slotProps={{ input: { 'aria-label': t('features.agentMemory.rememberMe', 'Remember things about me') } }}
            data-testid="agent-memory-opt-out"
          />
        }
        label={t('features.agentMemory.rememberMe', 'Remember things about me')}
      />
      <Box sx={{ display: 'flex', gap: '0.5rem' }}>
        <Button variant="outlined" size="small" onClick={() => void panel.exportAll()} disabled={panel.exporting || panel.total === 0} data-testid="agent-memory-export">
          {t('features.agentMemory.export', 'Export')}
        </Button>
        {panel.abilities.canDelete && (
          <Button variant="outlined" color="error" size="small" onClick={dialogs.openClear} disabled={panel.clearing || panel.total === 0} data-testid="agent-memory-clear">
            {t('features.agentMemory.clearAll', 'Clear all')}
          </Button>
        )}
      </Box>
    </Box>
  );
}

function MemoryPager({ page, total }: { readonly page: MemoryPanelModel['page']; readonly total: number }): ReactNode {
  if (!page.hasPrevious && !page.hasNext) return null;
  return (
      <Box sx={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
        <Button size="small" disabled={!page.hasPrevious} onClick={page.previous}>
          {t('features.agentMemory.previous', 'Previous')}
        </Button>
        <Typography variant="bodySmall" color="text.secondary">
          {t('features.agentMemory.range', '{{first}}–{{last}} of {{total}}', {
            first: page.offset + 1,
            last: Math.min(page.offset + MEMORY_PAGE_SIZE, total),
            total,
          })}
        </Typography>
        <Button size="small" disabled={!page.hasNext} onClick={page.next}>
          {t('features.agentMemory.next', 'Next')}
        </Button>
      </Box>
  );
}
