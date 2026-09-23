/**
 * "Does the credential this toolkit names still exist?" — the pure half of
 * the attached-toolkit credential warning (#937/ELITEA-1082,1083,1084,1085,
 * 1086,1098,1100).
 *
 * A toolkit stores its saved-credential choice inside its own settings blob,
 * under a `<type>_configuration` key holding `{elitea_title, private}` (the
 * shape `pages/toolkits/lib/credentialPicker.tsx`'s `StoredCredentialValue`
 * writes, and the same pair `toolkits.p13-credential-warnings.spec.ts` seeds).
 * Nothing normalises that blob on the way to an Agent's or a Pipeline's
 * attached-tool row — `features/agents/lib/versionTools.ts` passes the joined
 * `elitea_tools.settings` straight through — so the reference is readable from
 * the row itself and no extra per-toolkit read is needed.
 *
 * It lives in `entities/` because three slices need the same answer
 * (`features/agents` renders both the Agent and the Pipeline tool card, and
 * `features/chat-participants` the participant card) and
 * `no-sideways-features` forbids one feature slice to import another.
 */
/**
 * `entities/toolkit`'s `ConfigurationMode`, restated rather than imported:
 * `no-sideways-entities` forbids one entity slice to import another, and
 * these three are wire-adjacent literals a caller compares against (that
 * file's own header says the VALUES are the contract, not the symbol).
 */
const PICKER_MODE_TITLES: ReadonlySet<string> = new Set(['Manual_Title', 'Create_Personal_Title', 'Create_Project_Title']);

/** The `_configuration` suffix every credential-holding toolkit property carries. */
const CONFIGURATION_SUFFIX = '_configuration';

export interface ToolkitCredentialReference {
  /** The saved credential's `elitea_title` — the only identifier a toolkit stores. */
  readonly eliteaTitle: string;
  /** `github` for `github_configuration`, etc. — the credential TYPE, used for the banner's copy and its create link. */
  readonly credentialType: string;
  /** `true` when the toolkit points at a credential in the user's PERSONAL project rather than this one. */
  readonly isPrivate: boolean;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * The `{elitea_title, private}` reference a toolkit's settings hold, or
 * `null` when it holds none.
 *
 * The two picker MODES (`Manual_Title` / `Create_*_Title`) are references to
 * nothing at all — the user typed the credential's fields inline, or is part
 * way through creating one — so they answer `null` rather than "missing",
 * exactly as `configurationDoesNotMatchAnything` already excludes them on the
 * toolkit's own edit page.
 */
export function readToolkitCredentialReference(settings: unknown): ToolkitCredentialReference | null {
  if (!isRecord(settings)) return null;
  for (const [key, value] of Object.entries(settings)) {
    if (!key.endsWith(CONFIGURATION_SUFFIX) || !isRecord(value)) continue;
    const title = value['elitea_title'];
    if (typeof title !== 'string' || title.trim() === '') continue;
    if (PICKER_MODE_TITLES.has(title)) continue;
    return { eliteaTitle: title, credentialType: key.slice(0, -CONFIGURATION_SUFFIX.length), isPrivate: value['private'] === true };
  }
  return null;
}

/** The subset of a listed configuration this check reads — the same two places a title can sit that `configurationDoesNotMatchAnything` looks in. */
export interface CredentialTitleSource {
  readonly elitea_title?: string | undefined;
  readonly data?: Readonly<Record<string, unknown>> | undefined;
}

/** Every title the given configurations answer to, for `isToolkitCredentialMissing` below. */
export function collectCredentialTitles(configurations: readonly CredentialTitleSource[]): ReadonlySet<string> {
  const titles = new Set<string>();
  for (const configuration of configurations) {
    if (typeof configuration.elitea_title === 'string') titles.add(configuration.elitea_title);
    const nested = configuration.data?.['title'];
    if (typeof nested === 'string') titles.add(nested);
  }
  return titles;
}

/**
 * `true` only when the read has SETTLED and the reference names nothing in
 * it. An in-flight or failed read answers `false`: a warning is a claim about
 * the user's data, and "we could not look" is not evidence — the same
 * `isFetching` guard `configurationDoesNotMatchAnything` applies.
 */
export function isToolkitCredentialMissing(
  reference: ToolkitCredentialReference | null,
  titles: ReadonlySet<string> | undefined,
): boolean {
  if (reference === null || titles === undefined) return false;
  return !titles.has(reference.eliteaTitle);
}
