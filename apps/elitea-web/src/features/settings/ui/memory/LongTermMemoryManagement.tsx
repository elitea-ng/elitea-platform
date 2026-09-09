/**
 * LongTermMemoryManagement — the "Long-term Memory" accordion, the real
 * replacement for the "Coming soon" placeholder that used to sit under
 * Settings > Profile (`ui/profile/ProfileLongTermMemory.tsx`, #870).
 *
 * Composes `LongTermMemoryTable` + `LongTermMemoryFormDialog` over
 * `useLongTermMemoryManagement` (`lib/memory/`, which owns every query,
 * mutation and dialog-open state — split out to stay under the
 * component-complexity budget, same pattern `widgets/chat-box`'s
 * `useChatBoxActions` establishes). No hand-written client: the generated
 * `useListMemories` hook as-is for the read, the generated raw write
 * functions wrapped in local `useMutation`s for create/update/delete/
 * clear-all — same "generated client only" call `pages/settings/
 * Webhooks.tsx` makes and for the same reason (orval's `query.useQuery:
 * true` makes every generated `useXxx` a query-shaped hook regardless of
 * HTTP verb).
 *
 * MASTER TOGGLE. There is no separate persisted "long-term memory enabled"
 * flag on the server — #870's backend scope is per-ENTRY `enabled`, not a
 * second global switch (see tenant/0136_personal_memory_entries.sql's own
 * header). The master toggle here is therefore a BULK ACTION
 * (`useLongTermMemoryManagement.toggleAll`): flipping it fires one
 * `updateMemory` PUT per entry that does not already match the requested
 * state. Its own displayed value reflects whether ANY memory is currently
 * enabled — the honest question a single switch over N independent
 * booleans can answer.
 */
import { memo } from 'react';

import AddIcon from '@mui/icons-material/Add';
import SearchIcon from '@mui/icons-material/Search';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import FormControlLabel from '@mui/material/FormControlLabel';
import InputAdornment from '@mui/material/InputAdornment';
import Snackbar from '@mui/material/Snackbar';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { AccordionConstants } from '@/shared/lib/constants';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { useLongTermMemoryManagement } from '../../lib/memory/useLongTermMemoryManagement';
import { formatMemoryTags } from '../../lib/memory/longTermMemoryHelpers';
import { LongTermMemoryFormDialog } from './LongTermMemoryFormDialog';
import { LongTermMemoryTable } from './LongTermMemoryTable';

export interface LongTermMemoryManagementProps {
  readonly projectId?: string | undefined;
}

export const LongTermMemoryManagement = memo(function LongTermMemoryManagement({ projectId = '' }: LongTermMemoryManagementProps) {
  const memoryState = useLongTermMemoryManagement(projectId);

  return (
    <>
      <BasicAccordion
        data-testid="long-term-memory-section"
        showMode={AccordionConstants.AccordionShowMode.LeftMode}
        defaultExpanded
        slotSx={{ accordion: { background: 'transparent' } }}
        items={[
          {
            title: t('settings.longTermMemory.title', 'Long-term Memory'),
            content: (
              <Box sx={styles.accordionContent}>
                <Typography variant="bodyMedium" color="text.secondary">
                  {t(
                    'settings.longTermMemory.description',
                    'Facts Elitea remembers about you across every conversation in this project — not just the current one.',
                  )}
                </Typography>

                <Box sx={styles.controlsRow}>
                  <FormControlLabel
                    control={
                      <Switch
                        checked={memoryState.anyEnabled}
                        disabled={!memoryState.canWrite || memoryState.rows.length === 0 || memoryState.isBulkToggling}
                        onChange={(event) => void memoryState.toggleAll(event.target.checked)}
                        data-testid="long-term-memory-master-toggle"
                      />
                    }
                    label={t('settings.longTermMemory.masterToggle', 'Use my memories in chat')}
                  />
                  <Box sx={styles.controlsRowRight}>
                    <TextField
                      size="small"
                      placeholder={t('settings.longTermMemory.searchPlaceholder', 'Search memories')}
                      value={memoryState.search}
                      onChange={(event) => memoryState.setSearch(event.target.value)}
                      slotProps={{ input: { startAdornment: <InputAdornment position="start"><SearchIcon fontSize="small" /></InputAdornment> } }}
                      data-testid="long-term-memory-search"
                    />
                    {memoryState.canWrite && (
                      <Button
                        variant="outlined"
                        color="error"
                        size="small"
                        disabled={memoryState.rows.length === 0 || memoryState.isMutating}
                        onClick={() => memoryState.setConfirmClearAll(true)}
                        data-testid="long-term-memory-clear-all"
                      >
                        {t('settings.longTermMemory.clearAll', 'Clear all')}
                      </Button>
                    )}
                    {memoryState.canWrite && (
                      <Button
                        variant="contained"
                        size="small"
                        startIcon={<AddIcon fontSize="small" />}
                        onClick={memoryState.openCreate}
                        disabled={memoryState.isLoading}
                        data-testid="long-term-memory-add"
                      >
                        {t('settings.longTermMemory.add', 'Add memory')}
                      </Button>
                    )}
                  </Box>
                </Box>

                {memoryState.isBulkToggling && (
                  <Box sx={styles.bulkProgress}>
                    <CircularProgress size={16} />
                    <Typography variant="bodySmall2">{t('settings.longTermMemory.bulkToggling', 'Updating all memories…')}</Typography>
                  </Box>
                )}

                <LongTermMemoryTable
                  rows={memoryState.rows}
                  isLoading={memoryState.isLoading}
                  canWrite={memoryState.canWrite}
                  onEdit={memoryState.openEdit}
                  onToggleEnabled={memoryState.toggleEnabled}
                  onDelete={memoryState.deleteRow}
                />
              </Box>
            ),
          },
        ]}
      />
      <LongTermMemoryFormDialog
        open={memoryState.formOpen}
        isSaving={memoryState.isSavingForm}
        initialValues={
          memoryState.editingEntry
            ? {
                content: memoryState.editingEntry.content,
                tags: formatMemoryTags(memoryState.editingEntry.tags),
                enabled: memoryState.editingEntry.enabled,
              }
            : undefined
        }
        serverError={memoryState.formServerError}
        onClose={memoryState.closeForm}
        onSubmit={memoryState.submitForm}
      />
      <DeleteEntityModal
        open={memoryState.confirmClearAll}
        onClose={() => memoryState.setConfirmClearAll(false)}
        onConfirm={memoryState.runClearAll}
        name={t('settings.longTermMemory.clearAllEntityName', 'all of your memories in this project')}
        copy={{
          title: t('settings.longTermMemory.clearAllDialogTitle', 'Clear all memories?'),
          textContent: t('settings.longTermMemory.clearAllDialogText', 'Are you sure you want to delete '),
          confirmText: t('settings.longTermMemory.clearAllConfirm', 'Clear all'),
          cancelText: t('settings.longTermMemory.dialog.cancel', 'Cancel'),
        }}
        content={{ inline: t('settings.longTermMemory.dialog.deleteInline', '? This action cannot be undone.') }}
        data-testid="long-term-memory-clear-all-confirm-dialog"
      />
      <Snackbar
        open={memoryState.toast !== null}
        autoHideDuration={4000}
        onClose={memoryState.closeToast}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
      >
        {memoryState.toast ? (
          <Alert onClose={memoryState.closeToast} severity={memoryState.toast.severity} variant="filled">
            {memoryState.toast.message}
          </Alert>
        ) : undefined}
      </Snackbar>
    </>
  );
});

const styles = {
  accordionContent: {
    display: 'flex',
    flexDirection: 'column',
    gap: '1rem',
    paddingRight: '1rem',
  },
  controlsRow: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    flexWrap: 'wrap',
    gap: '0.75rem',
  },
  controlsRowRight: {
    display: 'flex',
    alignItems: 'center',
    gap: '0.5rem',
  },
  bulkProgress: {
    display: 'flex',
    alignItems: 'center',
    gap: '0.5rem',
    color: 'text.secondary',
  },
};
