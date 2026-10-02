/**
 * The saved-configuration rows a credential picker offers, for one schema
 * section and one set of accepted configuration types.
 *
 * Port of the baseline's `useCredentialsData`
 * (`apps/elitea-ui/src/[fsd]/features/credentials/lib/hooks/
 * useCredentialsData.hooks.js`). It reads the same two sources the baseline
 * reads, in the same order:
 *
 *  1. the SELECTED project, with its shared rows (`include_shared=true`);
 *  2. the user's PERSONAL project, whose rows carry `private: true`.
 *
 * The personal query is skipped when the caller asks for public rows only, when
 * no personal project is known, when the personal project IS the selected one,
 * and for the `vectorstorage` section — each of those is a baseline branch, not
 * a simplification.
 *
 * It lives in `pages/` because `features/toolkits` may not import
 * `features/credentials` (`no-sideways-features`) — see
 * `./credentialPicker.tsx` for the whole routing story.
 */
import { useCallback, useMemo } from 'react';

import { credentialUrl, type Credential } from '@/entities/credential';
import { normalizeCredentialPage, useConfigurationsList } from '@/features/credentials';

import { usePersonalProjectId } from './usePersonalProjectId';

/** The baseline's own page size for this query (`useCredentialsData.hooks.js`: `pageSize: 500`, `sharedLimit: 500`) — a picker lists every row, it does not paginate. */
const PICKER_PAGE_SIZE = 500;

/** The one section that has no personal half in the baseline. */
const VECTOR_STORAGE_SECTION = 'vectorstorage';

/**
 * One selectable row. Structurally assignable to `features/credentials`'
 * `CredentialOptionRow`, which is deliberately NOT re-exported from that
 * slice's barrel (its §3.5 20-symbol budget is full) — so the shape is
 * declared here and checked structurally at the `<CredentialsSelect>` call
 * site, the same technique `./sharepointAuthModals.tsx` already uses.
 */
export interface CredentialPickerRow {
  /**
   * The saved row's own id.
   *
   * The connection check needs it: every read path seals a stored secret as a
   * `{{secret.NAME}}` reference, so a credential can only be re-checked through
   * the SAVED-row route, which addresses the row by id and carries no body.
   */
  readonly id: string;
  readonly uuid?: string;
  readonly eliteaTitle: string;
  readonly isPrivate: boolean;
  readonly displayLabel: string;
  readonly type: string;
  /** The project the row itself belongs to — the test-connection call needs it, not the selected project. */
  readonly ownerProjectId: string | undefined;
  readonly data: Readonly<Record<string, unknown>>;
  /**
   * The provider address this credential names (`data.base_url`, else
   * `data.url`), and `undefined` when it names none.
   *
   * `CredentialOptionLabel` renders its "open in a new tab" action ONLY when
   * this is set — that action is how a user goes and fixes the credential a
   * row has just told them is refused. This field was missing here, so the
   * property was `undefined` for every row the toolkit form offers and the
   * control never rendered anywhere in the application: the selector existed
   * (`entities/credential`), `CredentialsSelect` forwarded the prop, and
   * nothing in between ever computed it.
   */
  readonly credentialUrl?: string;
}

export interface UseCredentialRowsParams {
  readonly projectId: string | undefined;
  readonly section: string;
  /** The property's `configuration_types`. Empty means "accept every type". */
  readonly configurationTypes: readonly string[];
  readonly onlyPublic: boolean;
}

export interface UseCredentialRowsResult {
  readonly rows: readonly CredentialPickerRow[];
  readonly hasFetchedData: boolean;
  readonly isFetching: boolean;
  readonly refresh: () => void;
}

function readTitle(credential: Credential): string {
  const fromData = credential.data?.['title'];
  if (credential.eliteaTitle !== undefined && credential.eliteaTitle.trim() !== '') return credential.eliteaTitle;
  return typeof fromData === 'string' ? fromData : '';
}

/** `configuration.label || configuration.elitea_title || configuration.data?.title`, exactly as the baseline builds its option label. */
function readDisplayLabel(credential: Credential, eliteaTitle: string): string {
  if (credential.label !== undefined && credential.label.trim() !== '') return credential.label;
  return eliteaTitle;
}

function toRows(items: readonly Credential[], isPrivate: boolean, accepted: ReadonlySet<string>): CredentialPickerRow[] {
  const rows: CredentialPickerRow[] = [];
  for (const item of items) {
    if (accepted.size > 0 && !accepted.has(item.type)) continue;
    const eliteaTitle = readTitle(item);
    // A row with no title cannot be selected: the value codec encodes the
    // empty string as "no selection", so such a row would silently clear the
    // field instead of picking anything.
    if (eliteaTitle === '') continue;
    rows.push({
      id: item.uid ?? item.id,
      ...(item.uuid !== undefined ? { uuid: item.uuid } : {}),
      eliteaTitle,
      isPrivate,
      displayLabel: readDisplayLabel(item, eliteaTitle),
      type: item.type,
      ownerProjectId: item.projectId,
      data: item.data ?? {},
      // Omitted rather than set to '' when the credential names no address:
      // the label renders the action on a truthy value, and an empty string
      // would open a new tab on nothing.
      ...(credentialUrl(item) !== '' ? { credentialUrl: credentialUrl(item) } : {}),
    });
  }
  return rows;
}

export function useCredentialRows(params: UseCredentialRowsParams): UseCredentialRowsResult {
  const { projectId, section, configurationTypes, onlyPublic } = params;
  const personalProjectId = usePersonalProjectId();

  const wantsPersonalRows =
    !onlyPublic && personalProjectId !== undefined && personalProjectId !== projectId && section !== VECTOR_STORAGE_SECTION;

  const projectQuery = useConfigurationsList(
    { projectId: projectId ?? '', section, pageSize: PICKER_PAGE_SIZE, sharedLimit: PICKER_PAGE_SIZE, includeShared: true },
    { enabled: projectId !== undefined },
  );
  const personalQuery = useConfigurationsList(
    { projectId: personalProjectId ?? '', section, pageSize: PICKER_PAGE_SIZE, sharedLimit: PICKER_PAGE_SIZE, includeShared: true },
    { enabled: wantsPersonalRows },
  );

  const projectData = projectQuery.data;
  const personalData = personalQuery.data;

  const rows = useMemo(() => {
    const accepted = new Set(configurationTypes);
    const projectPage = projectData === undefined ? undefined : normalizeCredentialPage(projectData);
    const personalPage = wantsPersonalRows && personalData !== undefined ? normalizeCredentialPage(personalData) : undefined;
    return [
      ...toRows(projectPage?.items ?? [], false, accepted),
      ...toRows(projectPage?.shared?.items ?? [], false, accepted),
      ...toRows(personalPage?.items ?? [], true, accepted),
    ];
  }, [projectData, personalData, wantsPersonalRows, configurationTypes]);

  const refetchProject = projectQuery.refetch;
  const refetchPersonal = personalQuery.refetch;
  const refresh = useCallback(() => {
    void refetchProject();
    if (wantsPersonalRows) void refetchPersonal();
  }, [refetchProject, refetchPersonal, wantsPersonalRows]);

  // `hasFetchedData` gates the picker's own "no selection" placeholder and its
  // auto-select. It must stay FALSE while a query is still pending, or a
  // saved value renders as "not found" for one frame before the rows land.
  const hasFetchedData = projectId !== undefined && !projectQuery.isPending && (!wantsPersonalRows || !personalQuery.isPending);

  return {
    rows,
    hasFetchedData,
    isFetching: projectQuery.isFetching || (wantsPersonalRows && personalQuery.isFetching),
    refresh,
  };
}
