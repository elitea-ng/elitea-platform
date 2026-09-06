import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import type { ConfigSchemaNode, ConfigSchemaSection, ConfigSchemaSubsection } from '@/features/credentials';
import { RadioButtonGroup } from '@/shared/ui/RadioButtonGroup';

import { CredentialSchemaField } from './CredentialFormFields';

const ANONYMOUS_OPTION = 'none';
const EMPTY_SUBSECTIONS: readonly ConfigSchemaSubsection[] = [];

export interface CredentialFormSectionProps {
  readonly sectionKey: string;
  readonly section: ConfigSchemaSection;
  readonly schemaProperties: Readonly<Record<string, ConfigSchemaNode>>;
  readonly schemaRequiredFields: readonly string[];
  readonly data: Readonly<Record<string, unknown>>;
  readonly fieldErrors: Readonly<Record<string, string>>;
  /** Threaded to `CredentialSchemaField` for the `'configuration'` picker. */
  readonly projectId: string;
  readonly onChange: (fieldKey: string, value: unknown) => void;
}

function hasValue(value: unknown): boolean {
  return value !== null && value !== undefined && value !== '';
}

function fieldsOf(subsection: ConfigSchemaSubsection | undefined): readonly string[] {
  return subsection?.fields ?? [];
}

function inferSelection(subsections: readonly ConfigSchemaSubsection[], data: Readonly<Record<string, unknown>>): string | undefined {
  let selected: ConfigSchemaSubsection | undefined;
  let selectedCount = 0;
  for (const subsection of subsections) {
    const count = fieldsOf(subsection).filter((field) => hasValue(data[field])).length;
    if (count > selectedCount) {
      selected = subsection;
      selectedCount = count;
    }
  }
  return selected?.name;
}

function visibleWhenMatches(property: ConfigSchemaNode | undefined, data: Readonly<Record<string, unknown>>): boolean {
  const condition = property?.['visible_when'];
  if (typeof condition !== 'object' || condition === null) return true;
  const field = (condition as Readonly<Record<string, unknown>>)['field'];
  const expected = (condition as Readonly<Record<string, unknown>>)['value'];
  if (typeof field !== 'string') return true;
  const current = data[field];
  if (typeof current === 'string' && typeof expected === 'string') {
    return current.toLowerCase() === expected.toLowerCase();
  }
  return current === expected;
}

/** Return true when the form can show the schema property. */
export function isCredentialPropertyVisible(
  property: ConfigSchemaNode | undefined,
  data: Readonly<Record<string, unknown>>,
): boolean {
  if (property?.['hidden'] === true || property?.metadata?.hidden === true) return false;
  return visibleWhenMatches(property, data);
}

/** Return all field keys that a section owns. */
export function credentialSectionFieldKeys(section: ConfigSchemaSection): readonly string[] {
  const keys = new Set<string>();
  for (const subsection of section.subsections ?? []) {
    for (const field of fieldsOf(subsection)) keys.add(field);
  }
  return [...keys];
}

function initialOption(
  sectionKey: string,
  section: ConfigSchemaSection,
  subsections: readonly ConfigSchemaSubsection[],
  data: Readonly<Record<string, unknown>>,
): string {
  const inferred = inferSelection(subsections, data);
  if (inferred) return inferred;
  if (sectionKey === 'auth' && section.required !== true) return ANONYMOUS_OPTION;
  return subsections[0]?.name ?? ANONYMOUS_OPTION;
}

function sectionTitle(sectionKey: string): string {
  return sectionKey.length === 0 ? sectionKey : `${sectionKey[0]?.toUpperCase() ?? ''}${sectionKey.slice(1)}`;
}

export function CredentialFormSection({
  sectionKey,
  section,
  schemaProperties,
  schemaRequiredFields,
  data,
  fieldErrors,
  projectId,
  onChange,
}: CredentialFormSectionProps): ReactNode {
  const subsections = section.subsections ?? EMPTY_SUBSECTIONS;
  const inferredOption = useMemo(() => initialOption(sectionKey, section, subsections, data), [sectionKey, section, subsections, data]);
  const [selectedOption, setSelectedOption] = useState(inferredOption);
  const didSelectOption = useRef(false);
  const cachedValues = useRef<Record<string, Record<string, unknown>>>({});

  useEffect(() => {
    if (!didSelectOption.current) setSelectedOption(inferredOption);
  }, [inferredOption]);

  const selectedSubsection = useMemo(
    () => subsections.find((subsection) => subsection.name === selectedOption),
    [selectedOption, subsections],
  );
  const selectedFields = fieldsOf(selectedSubsection);

  const options = useMemo(() => {
    const items = subsections.map((subsection) => ({ value: subsection.name, label: subsection.name }));
    return sectionKey === 'auth' && section.required !== true
      ? [{ value: ANONYMOUS_OPTION, label: 'Anonymous' }, ...items]
      : items;
  }, [sectionKey, section.required, subsections]);

  const selectOption = useCallback(
    (nextOption: string) => {
      didSelectOption.current = true;
      if (selectedSubsection) {
        cachedValues.current[selectedSubsection.name] = Object.fromEntries(
          fieldsOf(selectedSubsection).filter((field) => hasValue(data[field])).map((field) => [field, data[field]]),
        );
      }

      const nextFields = new Set(fieldsOf(subsections.find((subsection) => subsection.name === nextOption)));
      for (const field of credentialSectionFieldKeys(section)) {
        if (!nextFields.has(field)) onChange(field, null);
      }
      for (const [field, value] of Object.entries(cachedValues.current[nextOption] ?? {})) onChange(field, value);
      setSelectedOption(nextOption);
    },
    [data, onChange, section, selectedSubsection, subsections],
  );

  if (subsections.length === 0) return null;

  return (
    <Box sx={(theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(1), paddingTop: theme.spacing(1) })}>
      <Typography variant="bodySmall">{sectionTitle(sectionKey)}</Typography>
      {options.length > 1 && (
        <RadioButtonGroup
          aria-label={`${sectionTitle(sectionKey)} method`}
          value={selectedOption}
          items={options}
          onChange={selectOption}
          wrapRow
        />
      )}
      {selectedFields.map((fieldKey) => {
        const property = schemaProperties[fieldKey];
        if (!property || !isCredentialPropertyVisible(property, data)) return null;
        const required = schemaRequiredFields.includes(fieldKey) || section.required === true;
        return (
          <CredentialSchemaField
            key={fieldKey}
            fieldKey={fieldKey}
            property={property}
            value={data[fieldKey]}
            error={fieldErrors[fieldKey]}
            required={required}
            projectId={projectId}
            onChange={onChange}
          />
        );
      })}
    </Box>
  );
}
