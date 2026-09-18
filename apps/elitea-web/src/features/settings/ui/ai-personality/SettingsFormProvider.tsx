/**
 * SettingsFormProvider — the ONE save mechanism behind Settings › AI
 * Personality and Settings › Memory.
 *
 * Baseline: `EliteaUI/src/[fsd]/features/settings/ui/shared/
 * SettingsFormProvider.jsx` (same toasts: "Settings saved successfully" /
 * "Failed to save settings"). Both pages are auto-save-on-blur Formik forms
 * over one record, so the fetch, the Formik host, the PUT and the toasts live
 * here once; each page supplies only its own field layout as `children`.
 *
 * Nothing here is a second API client: the read is the generated
 * `useGetCurrentAuthor` query and the write is the generated
 * `updateCurrentAuthor` operation (both from
 * `shared/api/generated/social/social.ts`, the same endpoint pair
 * `pages/settings/Personalization.tsx` already uses), and the save
 * invalidates that query's own key so every OTHER reader re-baselines on
 * its own next mount. It does NOT force an immediate refetch of THIS
 * already-mounted query (`refetchType: 'none'` in `handleSubmit`) — see
 * that function's own comment for the data-loss bug that caused.
 *
 * LOCATION NOTE: this belongs in a neutral `ui/shared/` (as it does in the
 * baseline) rather than under `ui/ai-personality/`; it sits here only because
 * the unit that added these two pages owned no third directory. Same slice,
 * so `ui/memory/` importing it is an intra-slice import, not a cross-feature
 * one (R-L1).
 */
import type { ReactNode } from 'react';
import { memo, useCallback, useMemo, useRef, useState } from 'react';

import Alert from '@mui/material/Alert';
import Snackbar from '@mui/material/Snackbar';
import { useQueryClient } from '@tanstack/react-query';
import { Form, Formik, type FormikHelpers, type FormikProps } from 'formik';

import { ProfileValidationSchema } from '@/features/settings/lib/profile/profileUtils';
import { t } from '@/shared/i18n';
import {
  getGetCurrentAuthorQueryKey,
  updateCurrentAuthor,
  useGetCurrentAuthor,
} from '@/shared/api/generated/social/social';

import {
  type AuthorProfile,
  type SettingsProfileFormValues,
  buildAuthorUpdate,
  serializeSettingsProfile,
} from './settingsProfileForm';

const TOAST_AUTO_HIDE_MS = 3000;

/**
 * `true` when nothing in the form has changed since `saved` was captured.
 *
 * Compared field-by-field (not `JSON.stringify`, which is order-sensitive
 * for `personality_instructions` and would occasionally call two equal
 * records unequal) against exactly the shape `SettingsProfileFormValues`
 * declares — see this file's header comment on `handleSubmit` for why this
 * check exists.
 */
function valuesUnchangedSince(saved: SettingsProfileFormValues, current: SettingsProfileFormValues): boolean {
  if (
    saved.persona !== current.persona ||
    saved.context_enabled !== current.context_enabled ||
    saved.max_context_tokens !== current.max_context_tokens ||
    saved.preserve_recent_messages !== current.preserve_recent_messages ||
    saved.enable_context_editing !== current.enable_context_editing ||
    saved.enable_summarization !== current.enable_summarization
  ) {
    return false;
  }
  const savedInstructions = saved.personality_instructions;
  const currentInstructions = current.personality_instructions;
  const keys = Object.keys(savedInstructions);
  if (keys.length !== Object.keys(currentInstructions).length) return false;
  if (keys.some((key) => savedInstructions[key] !== currentInstructions[key])) return false;

  const savedLlm = saved.summary_llm_settings;
  const currentLlm = current.summary_llm_settings;
  return (
    savedLlm.instructions === currentLlm.instructions &&
    savedLlm.model_name === currentLlm.model_name &&
    savedLlm.model_project_id === currentLlm.model_project_id &&
    savedLlm.max_tokens === currentLlm.max_tokens
  );
}

export interface SettingsFormProviderProps {
  /** The page's field layout. Rendered inside the Formik context. */
  children: ReactNode;
  /**
   * Currently-selected project id. Used only as the fallback owner of the
   * summarization model when the saved profile names no project — the
   * baseline's `selectedProjectId` argument to `serializeProfileFormData`.
   */
  projectId?: string;
}

type ToastKind = 'success' | 'error' | null;

export const SettingsFormProvider = memo(({ children, projectId }: SettingsFormProviderProps) => {
  const { data } = useGetCurrentAuthor();
  const author = data?.data as AuthorProfile | undefined;
  const queryClient = useQueryClient();
  const [toast, setToast] = useState<ToastKind>(null);
  const formikRef = useRef<FormikProps<SettingsProfileFormValues> | null>(null);

  const initialValues = useMemo(
    () => serializeSettingsProfile(author, projectId),
    [author, projectId],
  );

  const handleSubmit = useCallback(
    async (values: SettingsProfileFormValues, helpers: FormikHelpers<SettingsProfileFormValues>) => {
      try {
        await updateCurrentAuthor(buildAuthorUpdate(author, values));
        // `refetchType: 'none'`: this INVALIDATES the cache (a later mount
        // of `useGetCurrentAuthor` anywhere else fetches fresh instead of
        // serving the stale entry) without forcing THIS already-mounted
        // query to refetch right now. That refetch used to run
        // unconditionally, and its response — landing after two network
        // round trips (`updateCurrentAuthor` plus the refetch itself), by
        // which point the user can have typed a further autosave-triggering
        // edit (a blur, a persona change) — fed straight into `initialValues`
        // below. With `enableReinitialize` on, Formik's OWN reset-on-
        // reinitialize effect (`formik.cjs.development.js`'s `resetForm()`,
        // called with NO arguments, which means "use `initialValues.current`")
        // then overwrote the CURRENT values with that now-stale fetch,
        // silently discarding the interim edit — traced with console/network
        // logging: the PUT this spec's "second blur" case waits for never
        // leaves the browser, because `dirty` had already been forced back
        // to false by the time the blur fired. `helpers.resetForm({ values
        // })` below re-baselines THIS form directly and carries the same
        // race for the same reason, so it is likewise guarded.
        await queryClient.invalidateQueries({ queryKey: getGetCurrentAuthorQueryKey(), refetchType: 'none' });
        // Only re-baseline when nothing changed the live form while this
        // save was in flight; otherwise leave it alone — it is still dirty,
        // and the interim edit's own blur/change already has its own
        // autosave queued.
        if (formikRef.current && valuesUnchangedSince(values, formikRef.current.values)) {
          helpers.resetForm({ values });
        }
        setToast('success');
      } catch {
        setToast('error');
      }
    },
    [author, queryClient],
  );

  const closeToast = useCallback(() => setToast(null), []);

  return (
    <>
      <Formik<SettingsProfileFormValues>
        innerRef={formikRef}
        enableReinitialize
        initialValues={initialValues}
        validationSchema={ProfileValidationSchema}
        onSubmit={handleSubmit}
      >
        <Form>{children}</Form>
      </Formik>
      <Snackbar
        open={toast !== null}
        autoHideDuration={TOAST_AUTO_HIDE_MS}
        onClose={closeToast}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
      >
        <Alert
          onClose={closeToast}
          severity={toast === 'error' ? 'error' : 'success'}
          variant="filled"
        >
          {toast === 'error'
            ? t('settings.saveError', 'Failed to save settings')
            : t('settings.saveSuccess', 'Settings saved successfully')}
        </Alert>
      </Snackbar>
    </>
  );
});

SettingsFormProvider.displayName = 'SettingsFormProvider';
