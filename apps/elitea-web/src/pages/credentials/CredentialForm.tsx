/**
 * pages/credentials/CredentialForm.tsx — the create/edit credential screen.
 * Ported from `apps/elitea-ui/src/pages/Credentials/CredentialForm.jsx`
 * (create) and `EditCredential.jsx` (edit) — the baseline splits these
 * across two thin page files around one shared body; this port keeps that
 * body as one component and lets `CreateCredential.tsx`/`EditCredential.tsx`
 * (this unit's other two page files) supply the differing `mode`.
 * Manifest ROUTE-023/024/025, ROUTE-063/064/065, COPY-465, ACT-040, ACT-041.
 *
 * DISCLOSED SCOPE (see this unit's final report for the full account):
 *  - No session/project store exists yet anywhere in this app, so
 *    `context` (project id, permissions, team-project flag) is entirely
 *    caller-supplied rather than read from a global store — the future
 *    route-wiring pass threads these from whatever R2/a later unit lands.
 *  - The data schema supports primitive fields and selectable sections.
 *    Nested object and array fields still use the simple field renderer.
 *  - The baseline's "auto-reveal the title field on an `elitea_title` API
 *    error" UX is dropped in favour of an always-editable title field —
 *    functionally equivalent (the title is always settable), a simpler
 *    interaction.
 *
 * Split across this file + `CredentialFormFields.tsx` (field widgets) +
 * `useCredentialFormController.ts` (state/mutation orchestration) purely to
 * keep every function under the §3.5 cyclomatic-complexity (≤12) and
 * file-length (≤400 lines) budgets — a single-file version of this screen
 * measured complexity 27 on the root component alone.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { CredentialsActions } from '@/features/credentials';

import { CredentialSchemaField } from './CredentialFormFields';
import {
  CredentialFormSection,
  credentialSectionFieldKeys,
  isCredentialPropertyVisible,
} from './CredentialFormSection';
import { CredentialTypeSelector } from './CredentialTypeSelector';
import { useCredentialFormController } from './useCredentialFormController';
import type { CredentialFormContext, CredentialFormMode, CredentialFormPrefill } from './useCredentialFormController';

export type { CredentialFormContext, CredentialFormMode, CredentialFormPrefill } from './useCredentialFormController';

export interface CredentialFormProps {
  readonly context: CredentialFormContext;
  readonly mode: CredentialFormMode;
  /** See `useCredentialFormController.ts`'s `onSaved` doc comment. */
  readonly onSaved: (savedId?: string) => void;
  readonly onDiscarded: () => void;
  readonly prefill?: CredentialFormPrefill;
  readonly onTypeChosen?: (type: string) => void;
}

export function CredentialForm(props: CredentialFormProps): ReactNode {
  const c = useCredentialFormController(props);
  const { mode, context, onDiscarded } = props;
  const sectionFieldKeys = new Set(Object.values(c.schemaSections).flatMap(credentialSectionFieldKeys));

  // Gate on the RESOLVED descriptor, not on the raw type string: an unknown
  // `:credentialType` in the URL must fall back to the picker rather than
  // render an empty form. Same condition the baseline uses —
  // `pages/Credentials/CreateCredential.jsx`'s `isEditing` (:132) requires
  // `Object.keys(initialValues).length > 0`, and `initialValues` is `{}`
  // whenever its `schema` lookup (:60) misses. While the available-types
  // query is still in flight the descriptor is also absent, so the picker
  // renders in its own loading state first and then yields to the form —
  // again matching the baseline, whose `schema` is equally undefined until
  // `configurationsAsSchema` resolves.
  if (mode.kind === 'create' && !c.typeDescriptor) {
    return (
      <CredentialTypeSelector
        configurationsData={c.availableTypes.data}
        isFetching={c.availableTypes.isFetching}
        onSelectType={c.chooseType}
      />
    );
  }

  return (
    <Box sx={containerSx}>
      <Typography variant="headingMedium">
        {mode.configurationMode ? t('credentials.form.configurationTitle', 'Configuration') : t('credentials.form.title', 'Credential')}
      </Typography>
      <TextField
        label={t('credentials.form.nameLabel', 'Name')}
        value={c.name}
        onChange={(event) => {
          c.setName(event.target.value);
        }}
        error={Boolean(c.fieldErrors['elitea_title'])}
        helperText={c.fieldErrors['elitea_title']}
        fullWidth
        variant="standard"
      />
      {context.isTeamProject && (
        <FormControlLabel
          control={
            <Switch
              checked={c.shared}
              onChange={(event) => {
                c.setShared(event.target.checked);
              }}
            />
          }
          label={t('credentials.form.sharedLabel', 'Shared with the team')}
        />
      )}
      {Object.entries(c.schemaProperties).map(([fieldKey, property]) => {
        if (sectionFieldKeys.has(fieldKey) || !isCredentialPropertyVisible(property, c.data)) return null;
        return (
          <CredentialSchemaField
            key={fieldKey}
            fieldKey={fieldKey}
            property={property}
            value={c.data[fieldKey]}
            error={c.fieldErrors[fieldKey]}
            required={c.schemaRequiredFields.includes(fieldKey)}
            projectId={context.projectId}
            onChange={c.setField}
          />
        );
      })}
      {Object.entries(c.schemaSections).map(([sectionKey, section]) => (
        <CredentialFormSection
          key={sectionKey}
          sectionKey={sectionKey}
          section={section}
          schemaProperties={c.schemaProperties}
          schemaRequiredFields={c.schemaRequiredFields}
          data={c.data}
          fieldErrors={c.fieldErrors}
          projectId={context.projectId}
          onChange={c.setField}
        />
      ))}
      {c.apiError && (
        <Typography
          variant="labelSmall"
          sx={errorTextSx}
        >
          {c.apiError}
        </Typography>
      )}
      {c.typeDescriptor?.has_test_connection && <TestConnectionBlock controller={c} />}
      <Box sx={actionsRowSx}>
        <CredentialsActions.TabBar
          isEditing={mode.kind === 'edit'}
          onSave={c.save}
          onDiscard={onDiscarded}
          canSave={c.canSave}
          isSaving={c.isSaving}
        />
        {mode.kind === 'edit' && (
          <CredentialsActions.Controls
            credentialName={c.name}
            canDelete={c.canDelete}
            isDeleting={c.isDeleting}
            onDelete={c.remove}
            {...(c.deleteDisabledReason !== undefined ? { deleteDisabledReason: c.deleteDisabledReason } : {})}
          />
        )}
      </Box>
    </Box>
  );
}

interface TestConnectionBlockProps {
  readonly controller: ReturnType<typeof useCredentialFormController>;
}

/** Split out of `CredentialForm`'s render to keep that function's complexity in budget. */
function TestConnectionBlock({ controller }: TestConnectionBlockProps): ReactNode {
  const label = controller.typeDescriptor?.check_connection_label ?? t('credentials.form.testConnection', 'Test connection');
  return (
    <Box sx={testConnectionRowSx}>
      <BaseBtn
        variant="secondary"
        disabled={controller.isTesting}
        onClick={controller.testConnection}
      >
        {label}
      </BaseBtn>
      {controller.testResult === 'success' && (
        <Typography
          variant="labelSmall"
          sx={successTextSx}
        >
          {t('credentials.form.testSuccessMessage', 'Connection successful')}
        </Typography>
      )}
      {controller.testResult === 'failure' && (
        <Typography
          variant="labelSmall"
          sx={errorTextSx}
        >
          {controller.testMessage || t('credentials.form.testFailed', 'Connection test failed')}
        </Typography>
      )}
      {/* NOT the error colour: a credential type this build has no checker
          for has not failed anything, and painting it red sends the user off
          to fix a credential that is fine. Same "not supported yet" wording
          the server sends, rendered in the neutral secondary text style the
          rest of this form uses for informational lines. */}
      {controller.testResult === 'unsupported' && (
        <Typography
          variant="labelSmall"
          color="text.secondary"
        >
          {controller.testMessage || t('credentials.form.testUnsupported', 'Checking connection is not supported yet for this configuration type.')}
        </Typography>
      )}
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(2), maxWidth: '40rem' });
const actionsRowSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: theme.spacing(1) });
const testConnectionRowSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: theme.spacing(1) });
const errorTextSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.status.rejected });
const successTextSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.status.publishedText });
