/**
 * State and handlers for `pages/admin/Configuration.tsx` (unit A14, issue #200).
 *
 * Separated from the view in the same shape as `./useAdminSchedulesPage.ts`, so
 * the page file is layout and this file is behaviour.
 *
 * Two decisions are worth naming.
 *
 * **Only CHANGED keys are sent.** `draft` holds the operator's edits and
 * nothing else; the form reads `{...serverValues, ...draft}`. The reference
 * keeps a full copy of the section and PUTs all of it, which means a save of one
 * card re-asserts every other field — and a field the server would now refuse
 * (a link that was stored before the scheme check existed) would make an
 * unrelated save fail. Sending the delta also makes "is there anything to save"
 * a fact rather than a deep comparison.
 *
 * **The baseline is the SERVER's answer, always.** After a successful save the
 * draft is cleared and the query is invalidated, so what the form shows next is
 * what the server stored — not what was typed. A page that keeps showing the
 * typed value after a save cannot distinguish a write that landed from one that
 * was silently transformed.
 */
import { useCallback, useMemo, useState } from 'react';

import { withoutBlankLinks, type ConfigLink } from './ConfigurationLinksEditor';
import { withoutBlankListEntries } from './ConfigurationListEditor';
import { listItemTypeFor } from './ConfigurationSectionForm';
import {
  ADMIN_CONFIG_PAGE_FEATURES,
  configFailureReason,
  configFailureStatus,
  useAdminConfigSections,
  useAdminConfigValues,
  useSaveAdminConfigValues,
  type AdminConfigField,
  type AdminConfigSection,
} from './api/adminConfigurationApi';

export interface AdminConfigurationPageState {
  readonly sections: readonly AdminConfigSection[];
  readonly isLoadingSections: boolean;
  readonly sectionsError: string | undefined;
  readonly activeSection: AdminConfigSection | undefined;
  readonly onSelectSection: (sectionId: string) => void;
  /** The server's reason this section cannot be edited, when it gave one. */
  readonly unavailableReason: string | undefined;
  readonly values: Readonly<Record<string, unknown>>;
  readonly isLoadingValues: boolean;
  readonly valuesError: string | undefined;
  readonly onFieldChange: (key: string, next: unknown) => void;
  readonly isDirty: boolean;
  readonly isSaving: boolean;
  readonly onSave: () => void;
  readonly onDiscard: () => void;
  readonly saveError: string | undefined;
  readonly savedAt: number | undefined;
  readonly onDismissSaved: () => void;
  readonly onDismissError: () => void;
}

/**
 * Strips blank rows out of every repeating field, matching the server's
 * tolerance.
 *
 * Both editors deliberately KEEP a blank row while editing — "Add entry" would
 * otherwise create a row that vanished on the same render — so dropping them is
 * this function's job, on the way out. It takes the field specs because a list
 * field is only recognisable from its declared element type; keying on the name,
 * the way `_links` is keyed, would have made the behaviour depend on what the
 * schema happened to call the field.
 */
function cleanForSave(
  draft: Record<string, unknown>,
  fields: readonly AdminConfigField[],
): Record<string, unknown> {
  const byKey = new Map(fields.map((field) => [field.key, field]));
  const cleaned: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(draft)) {
    if (key.endsWith('_links') && Array.isArray(value)) {
      cleaned[key] = withoutBlankLinks(value as ConfigLink[]);
      continue;
    }
    const field = byKey.get(key);
    if (field !== undefined && listItemTypeFor(field) !== undefined && Array.isArray(value)) {
      cleaned[key] = withoutBlankListEntries(value);
      continue;
    }
    cleaned[key] = value;
  }
  return cleaned;
}

/**
 * The shared machinery behind BOTH admin schema-driven pages.
 *
 * Configuration and Features differ in exactly one thing — which sections they
 * show — and that difference is a server-declared `page` on the section. So this
 * takes the page id and everything else is common: the same section list query,
 * the same per-section values query, the same delta-only save, the same
 * server-declared unavailability.
 *
 * This is the one place in the admin port where a whole page's behaviour was
 * genuinely reusable, and it is reused rather than copied. The reference has two
 * files of ~500 lines each that share their state machine by duplication, and
 * their `MOVED_TO_FEATURES`/`FEATURES_SECTIONS` lists have to stay each other's
 * complement by hand.
 *
 * NOT exported. Its only two callers are the wrappers directly below it, and an
 * export with no importer is what the dead-code gate exists to catch — the page
 * id is not a knob a call site should be choosing, precisely because the two
 * pages partitioning the sections between them is the invariant this whole
 * mechanism protects.
 */
function useAdminConfigSectionsPage(page: string | undefined): AdminConfigurationPageState {
  const sectionsQuery = useAdminConfigSections();
  const sections = useMemo(
    () => (sectionsQuery.data ?? []).filter((section) => (section.page ?? '') === (page ?? '')),
    [sectionsQuery.data, page],
  );

  const [selectedId, setSelectedId] = useState<string | undefined>(undefined);
  const [draft, setDraft] = useState<Record<string, unknown>>({});
  const [saveError, setSaveError] = useState<string | undefined>(undefined);
  const [savedAt, setSavedAt] = useState<number | undefined>(undefined);

  // The FIRST AVAILABLE section is the default, not simply the first. Opening on
  // a pane that can only display a refusal reads as a broken page, and on this
  // deployment the first section in schema order is one of the unavailable ones.
  const activeSection = useMemo((): AdminConfigSection | undefined => {
    if (selectedId !== undefined) {
      const chosen = sections.find((section) => section.id === selectedId);
      if (chosen !== undefined) return chosen;
    }
    // A section with a dedicated surface counts as available for this purpose,
    // even though it keeps its `unavailable_reason` for the plugin-config value
    // endpoints. Otherwise the page would open on a refusal while a fully
    // working editor sat one click away.
    //
    // The test is the SERVER-declared `managed_surface`, not the view's editor
    // registry: this file is behaviour and must not acquire a second copy of
    // which surfaces this build can render.
    return (
      sections.find((section) => section.unavailable_reason === undefined) ??
      sections.find((section) => (section.managed_surface ?? '') !== '') ??
      sections[0]
    );
  }, [sections, selectedId]);

  const isAvailable =
    activeSection !== undefined && (activeSection.unavailable_reason ?? '') === '';

  const valuesQuery = useAdminConfigValues(activeSection?.id, isAvailable);
  const serverValues = valuesQuery.data;

  const values = useMemo(
    (): Readonly<Record<string, unknown>> => ({ ...serverValues, ...draft }),
    [serverValues, draft],
  );

  const saveMutation = useSaveAdminConfigValues();

  const onSelectSection = useCallback((sectionId: string) => {
    setSelectedId(sectionId);
    // Edits belong to the section they were made in. Carrying them across would
    // send another section's keys, which the server refuses as unknown — an
    // accurate refusal for a request the operator never meant to make.
    setDraft({});
    setSaveError(undefined);
    setSavedAt(undefined);
  }, []);

  const onFieldChange = useCallback((key: string, next: unknown) => {
    setDraft((previous) => ({ ...previous, [key]: next }));
  }, []);

  const onDiscard = useCallback(() => {
    setDraft({});
    setSaveError(undefined);
  }, []);

  const onSave = useCallback(() => {
    if (activeSection === undefined) return;
    setSaveError(undefined);
    saveMutation.mutate(
      { sectionId: activeSection.id, values: cleanForSave(draft, activeSection.fields ?? []) },
      {
        onSuccess: () => {
          setDraft({});
          setSavedAt(Date.now());
        },
        onError: (error: unknown) => {
          setSaveError(configFailureReason(error) ?? 'save');
        },
      },
    );
  }, [activeSection, draft, saveMutation]);

  // A 501 on the VALUES read is the server saying the section is unavailable, so
  // it is reported as the reason rather than as a load failure. It should not
  // normally be reachable — the query is disabled for a section that declared
  // one — and reaching it means the schema and the value endpoint disagree,
  // which the operator is better told than shown as a spinner.
  const valuesError = useMemo((): string | undefined => {
    const error = valuesQuery.error;
    if (error == null) return undefined;
    return configFailureReason(error) ?? 'load';
  }, [valuesQuery.error]);

  return {
    sections,
    isLoadingSections: sectionsQuery.isLoading,
    sectionsError:
      sectionsQuery.error == null
        ? undefined
        : (configFailureReason(sectionsQuery.error) ?? 'load'),
    activeSection,
    onSelectSection,
    unavailableReason:
      activeSection?.unavailable_reason ??
      (configFailureStatus(valuesQuery.error) === 501
        ? configFailureReason(valuesQuery.error)
        : undefined),
    values,
    isLoadingValues: valuesQuery.isLoading && isAvailable,
    valuesError,
    onFieldChange,
    isDirty: Object.keys(draft).length > 0,
    isSaving: saveMutation.isPending,
    onSave,
    onDiscard,
    saveError,
    savedAt,
    onDismissSaved: useCallback(() => {
      setSavedAt(undefined);
    }, []),
    onDismissError: useCallback(() => {
      setSaveError(undefined);
    }, []),
  };
}

/**
 * Admin › Configuration: the sections that declare no page.
 *
 * The default — absence of `page` — belongs to Configuration rather than to
 * Features so that a section added on the server without thinking about
 * placement lands on the general page, not on the one that is about product
 * feature switches.
 */
export function useAdminConfigurationPage(): AdminConfigurationPageState {
  return useAdminConfigSectionsPage(undefined);
}

/**
 * Admin › Features: five sections (was six — `resources`/"Help Center"
 * moved back to Configuration, A2/ELITEA-0032, `config_schemas.go`'s
 * `resourcesSection` own doc comment has the full history).
 */
export function useAdminFeaturesPage(): AdminConfigurationPageState {
  return useAdminConfigSectionsPage(ADMIN_CONFIG_PAGE_FEATURES);
}
