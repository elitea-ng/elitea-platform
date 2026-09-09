/**
 * MemoryFormContent — the body of Settings › Memory.
 *
 * Baseline: `EliteaUI/src/[fsd]/features/settings/ui/memory/
 * MemoryFormContent.jsx`. Owns the page's auto-save wiring: the numeric and
 * text fields save when they lose focus, the toggles save on change.
 *
 * `LongTermMemoryManagement` (#870) is a SECOND, independent section below
 * context management — persistent, cross-conversation memory, not the
 * context-window controls above it (see that component's own header for the
 * distinction). It manages its own React Query state and does not
 * participate in this page's Formik form or its `PUT /social/author`
 * auto-save — the `onBlur` handler below still fires when focus leaves one
 * of its fields (it is a plain DOM listener on the wrapping `Box`), which
 * simply re-submits the Formik form's UNCHANGED existing values again; that
 * extra idle PUT is harmless but is why this section's own controls fire no
 * `requestSubmit()` of their own.
 *
 * `projectId` is a prop, threaded from `pages/settings/Memory.tsx` (which
 * already receives it from its route) rather than read here from
 * `widgets/app-shell`'s selected-project store directly — `features/` may
 * not import `widgets/` (R-L1, `.dependency-cruiser.cjs`'s
 * `no-upward-from-features` rule).
 */
import Box from '@mui/material/Box';

import { useFormikAutoSaveOnBlur } from '@/shared/lib/hooks/useFormikAutoSaveOnBlur';

import { MemoryContextManagement } from './MemoryContextManagement';
import { LongTermMemoryManagement } from './LongTermMemoryManagement';

export interface MemoryFormContentProps {
  readonly projectId?: string | undefined;
}

export function MemoryFormContent({ projectId }: MemoryFormContentProps) {
  const { onBlur, requestSubmit } = useFormikAutoSaveOnBlur();

  return (
    <Box sx={styles.wrapper} onBlur={onBlur}>
      <Box sx={styles.container} data-testid="memory-form-content">
        <MemoryContextManagement onAutoSaveRequested={requestSubmit} />
        <LongTermMemoryManagement projectId={projectId} />
      </Box>
    </Box>
  );
}

const styles = {
  wrapper: {
    display: 'flex',
    justifyContent: 'center',
    width: '100%',
  },
  container: {
    display: 'flex',
    flexDirection: 'column',
    gap: '0.5rem',
    padding: '1.5rem',
    maxWidth: '50rem',
    width: '100%',
  },
};
