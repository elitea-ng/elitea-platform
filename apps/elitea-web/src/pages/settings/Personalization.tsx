// @ts-nocheck
/**
 * Personalization page (settings tab) — replaces the old-app's
 * `pages/UserSettings/UserSettings.jsx` → `Profile.jsx` →
 * `ProfileFormContent.jsx` chain.
 *
 * Wire: `handleSubmit` → `PUT /social/author` → toast on success/error.
 */
import { memo, useCallback, useMemo, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';
import { Form, Formik, type FormikHelpers } from 'formik';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Snackbar from '@mui/material/Snackbar';
import Alert from '@mui/material/Alert';

import { t } from '@/shared/i18n';
import { getGetCurrentAuthorQueryKey, useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { eliteaFetch } from '@/shared/api/generated/mutator';

import { profileFeature } from '@/features/settings';
import type { ProfileFormValues } from '@/features/settings';

const { useDefaultModel, ProfileFormContent, ProfileValidationSchema, deserializeProfileFormData, serializeProfileFormData } = profileFeature;

// Shape returned by GET /social/author — SocialAuthorProfile zod schema.
interface AuthorData {
  id: string;
  name: string;
  email: string;
  avatar: string;
  description: string;
  personal_project_id: string;
  personalization?: Record<string, unknown>;
}

/** PUT /social/author — update current author profile. */
async function updateAuthorPayload(payload: {
  name?: string;
  description?: string;
  avatar?: string;
  personalization?: unknown;
}): Promise<void> {
  await eliteaFetch('/social/author', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
}

interface PersonalizationProps {
  /** Currently-selected project id — threaded down from the route. */
  projectId: string;
}

const Personalization = memo(({ projectId }: PersonalizationProps) => {
  const { data: authorResponse, isLoading, isFetching } = useGetCurrentAuthor();
  const authorData = authorResponse?.data as AuthorData | undefined;
  const { modelList, defaultModel } = useDefaultModel({ projectId });
  const queryClient = useQueryClient();

  const initialValues = useMemo<ProfileFormValues>(
    () => serializeProfileFormData(authorData, defaultModel),
    [authorData, defaultModel],
  );

  const [isSaving, setIsSaving] = useState(false);
  const [showSuccessToast, setShowSuccessToast] = useState(false);
  const [showErrorToast, setShowErrorToast] = useState(false);

  const handleSubmit = useCallback(
    async (values: ProfileFormValues, helpers: FormikHelpers<ProfileFormValues>) => {
      setIsSaving(true);
      try {
        // Full payload — personalization + the context-management and
        // summarization settings nested inside it (see
        // deserializeProfileFormData's doc comment for why they're nested
        // rather than sibling top-level keys). Sending only `personalization`
        // silently dropped every Context Management / Summarization edit.
        const payload = deserializeProfileFormData(values);

        await updateAuthorPayload(payload);

        // Refetch the just-saved profile and re-baseline Formik against the
        // submitted values so `dirty` goes false — otherwise
        // useFormikAutoSaveOnBlur keeps re-submitting on every later blur.
        await queryClient.invalidateQueries({ queryKey: getGetCurrentAuthorQueryKey() });
        helpers.resetForm({ values });

        setShowSuccessToast(true);
      } catch {
        setShowErrorToast(true);
      } finally {
        setIsSaving(false);
      }
    },
    [queryClient],
  );

  const handleCloseSuccessToast = useCallback(() => setShowSuccessToast(false), []);
  const handleCloseErrorToast = useCallback(() => setShowErrorToast(false), []);

  return (
    <Box sx={styles.container}>
      {/* No header row — `routes/_shell/settings/personalization.tsx`
        * already renders `DrawerPageHeader`. See `Preferences.tsx`. */}
      <Box sx={styles.content}>
        <Formik<ProfileFormValues>
          enableReinitialize
          initialValues={initialValues}
          validationSchema={ProfileValidationSchema}
          onSubmit={handleSubmit}
        >
          {({ isSubmitting }) => (
            <Form>
              <ProfileFormContent
                projectId={projectId}
                name={authorData?.name ?? ''}
                avatar={authorData?.avatar ?? ''}
                email={authorData?.email ?? ''}
                isFetching={isFetching || isLoading}
                modelList={modelList}
              />
              <Box sx={styles.saveBar}>
                <Button
                  type="submit"
                  variant="contained"
                  color="primary"
                  disabled={isSubmitting || isSaving}
                  startIcon={isSaving ? <CircularProgress size={16} /> : null}
                >
                  {t('settings.profile.save', 'Save changes')}
                </Button>
              </Box>
            </Form>
          )}
        </Formik>
      </Box>

      {/* Toast notifications */}
      <Snackbar
        open={showSuccessToast}
        autoHideDuration={3000}
        onClose={handleCloseSuccessToast}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
      >
        <Alert onClose={handleCloseSuccessToast} severity="success" variant="filled">
          {t('settings.profile.saveSuccess', 'Settings saved successfully')}
        </Alert>
      </Snackbar>
      <Snackbar
        open={showErrorToast}
        autoHideDuration={3000}
        onClose={handleCloseErrorToast}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
      >
        <Alert onClose={handleCloseErrorToast} severity="error" variant="filled">
          {t('settings.profile.saveError', 'Failed to save settings')}
        </Alert>
      </Snackbar>
    </Box>
  );
});

Personalization.displayName = 'Personalization';

export default Personalization;

const styles = {
  container: {
    display: 'flex',
    flexDirection: 'column',
    height: '100%',
    width: '100%',
  },
  content: {
    flex: 1,
    minHeight: 0,
    overflowY: 'auto',
    backgroundColor: 'background.tabPanel',
  },
  saveBar: {
    display: 'flex',
    justifyContent: 'flex-end',
    padding: '1rem 1.5rem',
    borderTop: '0.0625rem solid',
    borderColor: 'border.table',
  },
};
