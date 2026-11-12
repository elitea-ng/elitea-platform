// @ts-nocheck
/**
 * The page component for `/settings/create-personal-token` (issue #493). See
 * `./PersonalTokens.tsx` for why this directory exists and why the page is
 * not declared inside its own route file.
 */
/**
 * ROUTE-066 `/settings/create-personal-token` -> `CreatePersonalToken` page.
 *
 * Form for creating a new personal access token with:
 *  - Token name (validated: alphanumeric, underscore, hyphen)
 *  - Expiration period (dropdown: never, days, weeks, hours, minutes)
 *  - Expiration value (number input, shown when period != never)
 *  - Generated token display via `GeneratedTokenDialog` on success
 *
 * PROJECT BINDING (`spec-llm-project-scope` §4, ADR-0018): the token binds to
 * the project the SIDEBAR selects. The page offers no project control of its
 * own — it reads `useSelectedProjectStore`, the same store
 * `settings/tokens.tsx` reads, and only discloses the binding it will send.
 * `project_id` stays OPTIONAL on the wire: `bindableProjectId` drops a store
 * value that is not a positive integer, and `onSubmit` then OMITS the field
 * rather than sending `null`. Absent means unbound, and the server keeps
 * unbound as its default. The disclosure line and the §4 failure copy live in
 * `routes/-ui/TokenProjectNotice.tsx`.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/pages/settings/CreatePersonalToken.jsx`.
 *
 * Deviations:
 *  - Uses react-hook-form + zod (project standards; formik/yup not installed)
 *  - No `useNavBlocker` hook (Wave-2 concern), and no Redux
 *  - Uses `@/shared/i18n` for i18n, `DrawerPageHeader` and
 *    `GeneratedTokenDialog` from shared UI, and the query hooks from
 *    `entities/token/api/tokenApi`
 *  - `MAX_TOKEN_NAME_LENGTH` is a LOCAL override (768), not the constant of
 *    the same name `@/entities/token/model/constants` exports (64) — that
 *    file is outside this cluster's file scope. old-app parity is
 *    `MAX_VARIABLES_LENGTH` (apps/elitea-ui/src/common/constants.js:69 = 768);
 *    the entities constant needs its own follow-up fix for a single source
 *    of truth.
 */
import { useCallback, useMemo, useState } from 'react';

import { zodResolver } from '@hookform/resolvers/zod';
import { useForm, useWatch } from 'react-hook-form';
import * as z from 'zod';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Paper from '@mui/material/Paper';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate } from '@tanstack/react-router';

import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import { DiscardButton } from '@/shared/ui/DiscardButton';
import { GeneratedTokenDialog } from '@/features/settings/ui/personal-tokens/GeneratedTokenDialog';
import { t } from '@/shared/i18n';
import { TOKEN_NAME_PATTERN, TOKEN_EXPIRATION_OPTIONS, DEFAULT_TOKEN_EXPIRATION_VALUE } from '@/entities/token/model/constants';
import { useCreateTokenMutation, useListTokensQuery } from '@/entities/token/api/tokenApi';
import { bindableProjectId } from '@/entities/token/model/selectors';
import { createTokenFailureMessage, TokenCreateError, TokenProjectNotice } from '@/routes/-ui/TokenProjectNotice';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { useTheme } from '@mui/material/styles';

/** old-app parity: `MAX_VARIABLES_LENGTH` — see file-header deviation note. */
const MAX_TOKEN_NAME_LENGTH = 768;

/* ── zod validation schema ─────────────────────────────────────────────── */

const validationSchema = z.object({
  name: z
    .string()
    .min(1, t('entities.token.form.nameRequired', 'Name is required'))
    .max(MAX_TOKEN_NAME_LENGTH)
    .refine((v) => TOKEN_NAME_PATTERN.test(v), {
      message: t('entities.token.form.namePattern', 'Only alphanumeric characters, underscore and hyphen are allowed'),
    }),
  measure: z.string().min(1),
  expiration: z.coerce.number().min(1).optional(),
});

type FormValues = z.infer<typeof validationSchema>;

/* ── page ─────────────────────────────────────────────────────────────── */

export function CreatePersonalTokenPage() {
  const navigate = useNavigate();
  const [showDialog, setShowDialog] = useState(false);
  const [generatedToken, setGeneratedToken] = useState<{
    token: string;
    name: string;
  }>({ token: '', name: '' });
  const createMutation = useCreateTokenMutation();
  const isGenerating = createMutation.isPending;
  useListTokensQuery({ enabled: false });
  const theme = useTheme();
  const [submitError, setSubmitError] = useState<string | null>(null);

  /* The binding, read the same way `settings/tokens.tsx` reads it. The server
     re-checks membership in the create transaction (§4) — the sidebar
     selection is the input, not the authority. */
  const selectedProjectId = useSelectedProjectStore((s) => s.project?.id ?? '');
  const selectedProjectName = useSelectedProjectStore((s) => s.project?.name ?? '');
  const boundProjectId = bindableProjectId(selectedProjectId);

  const form = useForm<FormValues>({
    resolver: zodResolver(validationSchema),
    mode: 'onChange',
    defaultValues: {
      name: '',
      measure: 'days',
      expiration: DEFAULT_TOKEN_EXPIRATION_VALUE,
    },
  });

  const {
    register,
    handleSubmit,
    watch,
    control,
    formState: { errors, isValid },
  } = form;

  const measure = watch('measure');
  const name = watch('name');

  const onSubmit = useCallback(async (values: FormValues) => {
    const expires =
      values.measure === 'never'
        ? null
        : { measure: values.measure, value: values.expiration ?? DEFAULT_TOKEN_EXPIRATION_VALUE };

    setSubmitError(null);

    try {
      const resp = await createMutation.mutateAsync({
        name: values.name,
        expires,
        // SPREAD, never `project_id: null` — §4 makes "absent" the unbound case.
        ...(boundProjectId === undefined ? {} : { project_id: boundProjectId }),
      });
      setGeneratedToken({ token: resp.token, name: resp.name });
      setShowDialog(true);
    } catch (error) {
      setSubmitError(createTokenFailureMessage(error));
    }
  }, [createMutation, boundProjectId]);

  const expirationValue = useWatch({ name: 'expiration', control }) as number | null;
  const hasChanged = useMemo(
    () => name !== '' || measure !== 'days' || expirationValue !== DEFAULT_TOKEN_EXPIRATION_VALUE,
    [name, measure, expirationValue],
  );

  const onCancel = useCallback(() => {
    void navigate({ to: '/settings/tokens' });
  }, [navigate]);

  const nameError = errors.name?.message;
  const isAtCharacterLimit = name.length >= MAX_TOKEN_NAME_LENGTH;

  const isGenerateDisabled = !isValid || isGenerating || !hasChanged;

  const styles = getStyles(theme);

  return (
    <>
      <Paper
        elevation={0}
        sx={styles.root}
      >
        <DrawerPageHeader
          title={t('entities.token.form.pageTitle', 'New Token')}
          extraContent={
            <Box sx={styles.headerRight}>
              <Button
                variant="elitea"
                color="primary"
                disabled={isGenerateDisabled}
                onClick={() => void handleSubmit(onSubmit)()}
              >
                {t('entities.token.form.generate', 'Generate')}
                {isGenerating && (
                  <CircularProgress
                    size={16}
                    sx={styles.loadingIndicator}
                  />
                )}
              </Button>
              <DiscardButton
                disabled={isGenerating || !hasChanged}
                onDiscard={onCancel}
                title={t('entities.token.form.discard', 'Discard')}
                alertContent={t(
                  'entities.token.form.discardConfirm',
                  'There are unsaved changes. Are you sure you want to discard them?',
                )}
              />
            </Box>
          }
        />
        <Box sx={styles.content}>
          <Box sx={styles.formWrapper}>
            <form onSubmit={() => void handleSubmit(onSubmit)()}>
              <Box sx={styles.formFields}>
                {/* Name field */}
                <Box sx={styles.nameField}>
                  <input
                    style={styles.input}
                    id="name"
                    {...register('name')}
                    placeholder={t('entities.token.form.name', 'Name')}
                    maxLength={MAX_TOKEN_NAME_LENGTH}
                    aria-label={t('entities.token.form.name', 'Name')}
                  />
                  {nameError && (
                    <Typography
                      variant="bodySmall"
                      color="error"
                      sx={styles.helperText}
                    >
                      {nameError}
                    </Typography>
                  )}
                  {isAtCharacterLimit && !nameError && (
                    <Typography
                      variant="bodySmall"
                      color="text.secondary"
                      sx={styles.helperText}
                    >
                      {t('entities.token.form.charLimit', `Maximum character limit reached (${MAX_TOKEN_NAME_LENGTH})`, { maxLength: MAX_TOKEN_NAME_LENGTH })}
                    </Typography>
                  )}
                </Box>

                {/* Expiration row */}
                <Box sx={styles.expirationRow}>
                  <Box sx={styles.measureField}>
                    <Box
                      component="select"
                      id="measure"
                      {...register('measure')}
                      sx={styles.select}
                      aria-label={t('entities.token.form.expirationPeriod', 'Expiration period')}
                    >
                      {TOKEN_EXPIRATION_OPTIONS.map((opt) => (
                        <option key={opt.value} value={opt.value}>
                          {opt.label}
                        </option>
                      ))}
                    </Box>
                  </Box>
                  {measure !== 'never' && (
                    <Box sx={styles.valueField}>
                      <input
                        style={styles.numberInput}
                        id="expiration"
                        type="number"
                        {...register('expiration')}
                        min={1}
                        aria-label={t('entities.token.form.expirationValue', 'Value')}
                      />
                    </Box>
                  )}
                </Box>

                {/* Project binding — the sidebar selection, fixed at creation (§4).
                    The name comes from the id the request carries, so a dropped
                    store value reads as unbound here too. `||`, not `??`: the
                    store gives `''` for a missing name, and the id labels it
                    better than an empty string does. */}
                <TokenProjectNotice projectName={boundProjectId === undefined ? null : selectedProjectName || selectedProjectId} />

                <TokenCreateError message={submitError} />
              </Box>
            </form>
          </Box>
        </Box>
      </Paper>

      <GeneratedTokenDialog
        open={showDialog}
        token={generatedToken.token}
        name={generatedToken.name}
        onClose={() => {
          setShowDialog(false);
          onCancel();
        }}
      />
    </>
  );
}

const getStyles = (theme: Theme): {
  root: SxProps<Theme>;
  headerRight: SxProps<Theme>;
  content: SxProps<Theme>;
  formWrapper: SxProps<Theme>;
  formFields: SxProps<Theme>;
  nameField: SxProps<Theme>;
  expirationRow: SxProps<Theme>;
  measureField: SxProps<Theme>;
  valueField: SxProps<Theme>;
  loadingIndicator: SxProps<Theme>;
  input: SxProps<Theme>;
  select: SxProps<Theme>;
  numberInput: SxProps<Theme>;
  helperText: SxProps<Theme>;
} => {
  const baseInput: SxProps<Theme> = {
    width: '100%',
    border: 'none',
    borderBottom: '1px solid',
    borderBottomColor: theme.vars.palette.border.lines,
    padding: '0.5rem 0',
    fontSize: ({ typography }) => typography.headingMedium.fontSize,
    outline: 'none',
    background: 'transparent',
  };

  return {
    root: {
      display: 'flex',
      flexDirection: 'column',
      height: '100%',
      overflow: 'hidden',
    },
    headerRight: {
      display: 'flex',
      alignItems: 'center',
      gap: '0.5rem',
    },
    content: {
      padding: '1.5rem',
      display: 'flex',
      justifyContent: 'center',
      alignItems: 'flex-start',
      flex: 1,
    },
    formWrapper: {
      marginTop: '1.25rem',
      width: '100%',
      maxWidth: '45rem',
    },
    formFields: {
      display: 'flex',
      flexDirection: 'column',
      alignItems: 'center',
      gap: '1.25rem',
    },
    nameField: {
      width: '17.875rem',
    },
    expirationRow: {
      display: 'flex',
      flexDirection: 'row',
      alignItems: 'flex-end',
      gap: '1rem',
      width: '17.875rem',
    },
    measureField: {
      width: '12.5rem',
    },
    valueField: {
      width: '5.375rem',
      paddingTop: '0.125rem',
    },
    loadingIndicator: {
      marginLeft: '0.5rem',
    },
    input: baseInput,
    // The open list is the browser's popup: inherited light text on its light
    // background was unreadable in the dark theme. Colour both explicitly.
    select: {
      ...baseInput,
      cursor: 'pointer',
      appearance: 'auto',
      color: theme.vars.palette.text.primary,
      colorScheme: 'light',
      ...theme.applyStyles('dark', { colorScheme: 'dark' }),
      '& option': {
        color: theme.vars.palette.text.primary,
        backgroundColor: theme.vars.palette.background.paper,
      },
    },
    numberInput: {
      ...baseInput,
      width: '100%',
      textAlign: 'center',
    },
    helperText: {
      marginTop: '0.25rem',
      fontSize: ({ typography }) => typography.bodySmall.fontSize,
    },
  };
};
