/**
 * pages/credentials/MicrosoftCredentialFields.tsx — the fields only the Microsoft
 * credentials (Outlook, Teams, Microsoft Graph, Dataverse) have (ADR-0057, S1d):
 *
 *  - the Default app picker (`ui_component: oauth_app_select`): the apps an administrator
 *    registered that serve this type and project. The credential stores the app's id; the
 *    client, secret and authority stay in Main;
 *  - the scope tick boxes (`ui_component: checkbox_list`) with the consent note;
 *  - which of "Default app" / "Custom app" the form offers: an empty registry shows only
 *    Custom app, and an administrator who turned `microsoft.custom_apps` off leaves only
 *    the Default apps.
 */
import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import FormGroup from '@mui/material/FormGroup';
import FormHelperText from '@mui/material/FormHelperText';
import Typography from '@mui/material/Typography';
import { useQuery } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';

import { listMicrosoftAppOffers } from '@/shared/api/generated/configurations/configurations';
import type { MicrosoftAppOffers } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { SingleSelect } from '@/shared/ui/SingleSelect';

import { SchemaField } from '@/features/credentials';
import type { ConfigSchemaNode, ConfigSchemaSubsection } from '@/features/credentials';

function numericProjectId(projectId: string): number | undefined {
  const id = Number(projectId);
  return Number.isInteger(id) && id > 0 ? id : undefined;
}

/** The Default apps one credential type may use in this project. Same key as the toolkit form's read, so both share one request. */
function useMicrosoftOffers(credentialType: string | undefined, projectId: string): UseQueryResult<MicrosoftAppOffers, Error> {
  const id = numericProjectId(projectId);
  return useQuery({
    queryKey: ['microsoft-app-offers', id, credentialType] as const,
    enabled: id !== undefined && credentialType !== undefined && credentialType !== '',
    retry: false,
    staleTime: 30_000,
    queryFn: async ({ signal }) =>
      unwrapBody(await listMicrosoftAppOffers(id ?? 0, { credential_type: credentialType ?? '' }, { signal })) as MicrosoftAppOffers,
  });
}

/** A value the person (or a stored credential) filled in. */
export function hasValue(value: unknown): boolean {
  return value !== null && value !== undefined && value !== '';
}

/** The field of this subsection that picks a Default app, if it has one. */
function appFieldOf(subsection: ConfigSchemaSubsection, schemaProperties: Readonly<Record<string, ConfigSchemaNode>>): string | undefined {
  return (subsection.fields ?? []).find((field) => schemaProperties[field]?.['ui_component'] === 'oauth_app_select');
}

export interface OfferedSubsections {
  readonly subsections: readonly ConfigSchemaSubsection[];
  /** Fields whose value does not say which subsection the credential uses (see `inferenceIgnoredFields`). */
  readonly ignored: ReadonlySet<string>;
  /** The ids of the Default apps on offer, in the registry's order. */
  readonly appIds: readonly string[];
}

const NO_FIELDS: ReadonlySet<string> = new Set();

export interface OfferedSubsectionsParams {
  readonly subsections: readonly ConfigSchemaSubsection[];
  readonly schemaProperties: Readonly<Record<string, ConfigSchemaNode>>;
  readonly data: Readonly<Record<string, unknown>>;
  readonly credentialType: string | undefined;
  readonly projectId: string;
}

/**
 * The auth subsections this form offers. A section with no Default app field is returned as it is
 * (and asks the registry nothing). One with such a field offers "Default app" only when an app is on
 * offer (or the credential already names one), and "Custom app" unless the administrator turned it
 * off. A subsection the credential already holds values in is never hidden, so it stays editable.
 */
export function useOfferedSubsections({ subsections, schemaProperties, data, credentialType, projectId }: OfferedSubsectionsParams): OfferedSubsections {
  const usesApps = subsections.some((subsection) => appFieldOf(subsection, schemaProperties) !== undefined);
  const offers = useMicrosoftOffers(usesApps ? credentialType : undefined, projectId);
  const appIds = useMemo(() => (offers.data?.apps ?? []).map((app) => app.id), [offers.data]);
  const customAllowed = offers.data?.custom_app_allowed !== false;
  const loading = usesApps && offers.isLoading;

  return useMemo(() => {
    if (!usesApps) return { subsections, appIds, ignored: NO_FIELDS };
    // Until the registry answers, offer nothing: showing Custom app first would flash fields a Default-app-only deployment never offers.
    if (loading) return { subsections: [], appIds, ignored: NO_FIELDS };
    // A value both subsections hold (the scopes), or one still at its default, does not say which one the credential uses.
    const ignored = inferenceIgnoredFields(subsections, schemaProperties, data);
    const offered = subsections.filter((subsection) => {
      const fields = subsection.fields ?? [];
      if (fields.some((field) => !ignored.has(field) && hasValue(data[field]))) return true;
      if (appFieldOf(subsection, schemaProperties) !== undefined) return appIds.length > 0;
      return customAllowed || appIds.length === 0;
    });
    return { subsections: offered, appIds, ignored };
  }, [usesApps, loading, subsections, data, appIds, customAllowed, schemaProperties]);
}

/**
 * The fields of an auth section whose value says nothing about which subsection the person chose: a
 * field both subsections hold (the scopes), and one still at the schema's own default (the refresh
 * switch). Only a section with a Default app field asks; the others infer as they always did.
 */
function inferenceIgnoredFields(
  subsections: readonly ConfigSchemaSubsection[],
  schemaProperties: Readonly<Record<string, ConfigSchemaNode>>,
  data: Readonly<Record<string, unknown>>,
): ReadonlySet<string> {
  if (!subsections.some((subsection) => appFieldOf(subsection, schemaProperties) !== undefined)) return NO_FIELDS;
  const seen = new Set<string>();
  const ignored = new Set<string>();
  for (const field of subsections.flatMap((subsection) => subsection.fields ?? [])) {
    if (seen.has(field)) ignored.add(field);
    seen.add(field);
  }
  for (const field of seen) {
    const fallback = schemaProperties[field]?.default;
    if (fallback !== undefined && JSON.stringify(fallback) === JSON.stringify(data[field])) ignored.add(field);
  }
  return ignored;
}

export interface MicrosoftAppFieldProps {
  readonly label: string;
  readonly required: boolean;
  readonly credentialType: string | undefined;
  readonly projectId: string;
  /** The credential's `oauth_app_id`. */
  readonly value: unknown;
  readonly onChange: (appId: string) => void;
}

/** The Default app picker: one option per registered app that serves this type and project. */
export function MicrosoftAppField({ label, required, credentialType, projectId, value, onChange }: MicrosoftAppFieldProps): ReactNode {
  const offers = useMicrosoftOffers(credentialType, projectId);
  const apps = offers.data?.apps ?? [];
  const current = typeof value === 'string' ? value : '';
  // An app the credential names that is no longer offered (disabled, or the project lost it) stays
  // visible, so the form does not read as "nothing chosen" for a credential that still resolves it.
  const missing = current !== '' && !apps.some((app) => app.id === current);
  const options = [
    ...apps.map((app) => ({ label: app.name, value: app.id })),
    ...(missing ? [{ label: t('toolkits.oauthApp.unavailable', 'Default app (not available)'), value: current }] : []),
  ];
  return (
    <SingleSelect
      label={label}
      required={required}
      value={current}
      options={options}
      onChange={(next) => {
        // A Default app is always one of the apps on offer: an empty pick is never written back as "nothing chosen".
        if (next !== '') onChange(next);
      }}
    />
  );
}

export interface CheckboxListFieldProps {
  readonly label: string;
  readonly property: ConfigSchemaNode | undefined;
  readonly value: unknown;
  readonly error: string | undefined;
  readonly onChange: (scopes: string[]) => void;
}

function selectedOf(value: unknown): ReadonlySet<string> {
  if (!Array.isArray(value)) return new Set();
  return new Set(value.filter((entry): entry is string => typeof entry === 'string'));
}

/** Tick boxes with a line of help each, and the schema's note (what Microsoft does with the ticked boxes) above them. */
export function CheckboxListField({ label, property, value, error, onChange }: CheckboxListFieldProps): ReactNode {
  const options = SchemaField.checkboxOptions(property) ?? [];
  const selected = selectedOf(value);
  const note = property?.description;
  return (
    <Box sx={listSx}>
      <Typography variant="bodySmall">{label}</Typography>
      {note !== undefined && note !== '' && (
        <Typography
          variant="labelSmall"
          color="text.secondary"
          sx={noteSx}
          data-testid="microsoft-scopes-note"
        >
          {note}
        </Typography>
      )}
      <FormGroup>
        {options.map((option) => (
          <Box key={option.value}>
            <FormControlLabel
              label={option.label}
              control={
                <BaseCheckbox
                  checked={selected.has(option.value)}
                  onChange={(event) => {
                    const next = new Set(selected);
                    if (event.target.checked) next.add(option.value);
                    else next.delete(option.value);
                    // Keep the list's own order, so a stored value does not depend on the order of the clicks.
                    onChange(options.map((entry) => entry.value).filter((name) => next.has(name)));
                  }}
                />
              }
            />
            {option.description !== '' && (
              <Typography
                variant="labelSmall"
                color="text.secondary"
                sx={optionNoteSx}
              >
                {option.description}
              </Typography>
            )}
          </Box>
        ))}
      </FormGroup>
      {error !== undefined && <FormHelperText error>{error}</FormHelperText>}
    </Box>
  );
}

const listSx = { display: 'flex', flexDirection: 'column', gap: 0.5 } as const;
const noteSx = { whiteSpace: 'pre-line' } as const;
const optionNoteSx = { display: 'block', marginLeft: 4, marginTop: -0.5 } as const;
