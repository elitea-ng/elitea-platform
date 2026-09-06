/**
 * Query-param schema registry (spec §8.2, decisions D-record P1 errata;
 * P1's manifest `PARAM-001`..`PARAM-108`, 45 distinct keys — supersedes the
 * spec's illustrative 10-row §8.2 table per the R1 task brief).
 *
 * One zod schema per distinct key, each with an EXPLICIT default (spec
 * §8.2 rule: "each row gets a zod schema... with an explicit default").
 * Route files compose these via `pickParams()` into their own
 * `validateSearch`; `common.ts` composes the "on any" (shell-wide) subset.
 *
 * This file is under `src/routes/-search/` — the leading `-` is TanStack
 * Router's own file-based-routing "ignore" prefix (verified against the
 * installed `@tanstack/router-generator@1.168.23`, `routeFileIgnorePrefix`
 * defaults to `"-"`), so this directory is never scanned as route files.
 *
 * Every key here is documented with the manifest item(s) that require it.
 * "[write-only URL contract]" keys (per REPRODUCE.md choice 4) are still
 * validated the same as read keys: the shape is parity-relevant even
 * without an in-app reader.
 *
 * PRE-PROCESSING NOTE (verified against the installed
 * `@tanstack/router-core@1.170.18`'s `parseSearchWith`/`defaultParseSearch`,
 * `searchParams.js`): the router's DEFAULT search parser attempts
 * `JSON.parse` on every raw query value and keeps the parsed result when it
 * succeeds. `?createSecret=1` therefore arrives at `validateSearch` as the
 * NUMBER `1`, not the string `"1"` — a real behavioural difference from the
 * old app's plain `URLSearchParams.get()` (always a string or `null`).
 * `toScalarString`/`toStringArray` below normalise JSON-parsed primitives
 * back to the string shape every schema BUILT VIA THE `flag()`/`text()`/
 * `list()` HELPERS is written against, so `z.enum(['0','1'])`/`z.string()`
 * see the same value old app's parity baseline would have. Verified with a
 * scratch parse of `?createSecret=1` and `?bucket=123` against the
 * installed parser: both come back as numbers without this normalisation.
 * The JSON literal `null` (e.g. `?createSecret=null`) is likewise
 * `JSON.parse`d successfully — to the real JS value `null`, not
 * `undefined` — so `toScalarString`/`toStringArray` also fold `null` back
 * to `undefined`, the "absent" shape every `.catch()`/schema default below
 * is written against.
 *
 * NOT every schema in the registry goes through this normalisation: four
 * fields — `sort_order`, `viewMode`, `view` (bare `z.enum(...)`) and
 * `page_size` (bare `z.coerce.number()`) — are declared directly, without
 * a `z.preprocess(toScalarString, ...)` wrapper. This is intentional, not
 * an oversight: for the three enums, no JSON-coerced number/boolean string
 * (e.g. `"true"`, `"1"`) is ever a valid member of the enum's value set
 * regardless of whether it went through `toScalarString` first, so
 * `.catch()` absorbs the mismatch either way and the wrapper would be
 * inert; for `page_size`, `z.coerce.number()` already runs its own
 * `Number(...)` coercion on whatever the router's `JSON.parse` produced
 * (string, number, or single-element array — `Number(['20'])` is `20`,
 * same as a `toScalarString`-then-`Number` pass would give), so a separate
 * scalar-string pass would be redundant for the scalar/single-element
 * shapes this field actually receives. (A multi-element array is the one
 * shape where the two paths would disagree — `Number(['1','2'])` is `NaN`,
 * where `toScalarString` would instead take the first element — but
 * `page_size` is a single value, never repeated in a URL, so that shape
 * does not occur in practice; either way `.catch(20)` absorbs a `NaN`.) Do
 * not read this comment as "every schema sees the same normalised value"
 * — only the `flag()`/`text()`/`list()`-built ones do; these four rely on
 * their own type coercion instead.
 *
 * CRASH-SAFETY NOTE: every schema in this registry is `<inner>.catch(x).prefault(x)`,
 * never a bare `.default()`. `.default()` only substitutes when the raw
 * input is `undefined` — it does NOT catch a validation failure on a
 * defined-but-invalid value (e.g. `z.enum(['0','1'])` given `"open"`, or
 * given `null` before the fold above), so a malformed query value used to
 * throw an uncaught `ZodError` out of `validateSearch` (TanStack Router
 * calls `.parse()`, not `.safeParse()`, and no ancestor route declares an
 * `errorComponent` for a `SearchParamError`). `.catch(x)` fixes that half:
 * it substitutes `x` on ANY validation failure, `undefined` input included,
 * so a malformed value now silently falls back to the schema's documented
 * default instead of crashing the screen (spec §8.2: "a malformed value is
 * rejected without crashing the screen").
 *
 * The `.prefault(x)` half fixes a TYPE-only regression `.catch()` alone
 * introduces: zod's `$ZodCatchInternals` types a `.catch()` schema's INPUT
 * as the wrapped schema's own input type, unlike `.default()`
 * (`$ZodDefault`) whose input type is explicitly `T | undefined`. A bare
 * `z.enum([...]).catch(x)` therefore stops being "optional" in the object
 * schema's inferred INPUT type, which made TanStack Router's generated
 * types require an explicit `search` on every `redirect()`/`navigate()`
 * call targeting a route that inherits `commonSearchSchema` (verified:
 * `npx tsc --noEmit` broke on every such call site the moment `.default()`
 * became a bare `.catch()` here). `$ZodPrefaultInternals` types ITS input as
 * `core.input<T> | undefined` (T being the `.catch()`-wrapped schema),
 * restoring the optional-input typing `.default()` used to give "for free".
 * At runtime `.prefault(x)` only substitutes `x` for `undefined` input,
 * same as `.default()` — but then re-runs the substituted value through the
 * wrapped schema rather than returning it verbatim, so undefined input
 * still ends up going through `.catch()` and landing on the exact same `x`;
 * chaining it after `.catch(x)` costs nothing at runtime and buys back the
 * type.
 */
import { z } from 'zod';

/** Normalises a JSON-parsed scalar (number/boolean) back to a string; the JSON literal `null` and `undefined` both normalise to `undefined` ("absent"); arrays take their first element; `undefined` passes through so `.catch()` still applies. */
function toScalarString(value: unknown): unknown {
  if (value === undefined || value === null) return undefined;
  if (Array.isArray(value)) return value.length > 0 ? toScalarString(value[0]) : undefined;
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  return value;
}

/**
 * Normalises a scalar or JSON-parsed array into a string array; the JSON
 * literal `null` and `undefined` both normalise to `undefined` ("absent").
 * Only genuinely scalar entries (string/number/boolean) are coerced — same
 * as `toScalarString`. If ANY entry is a non-scalar (object, array, nested
 * `null`), the whole value is malformed: this returns `undefined` so the
 * outer `.catch([])` substitutes the documented default instead of
 * stringifying garbage (`String({})` -> `"[object Object]"`) into data that
 * would otherwise pass `z.array(z.string())` as if it were valid.
 */
function toStringArray(value: unknown): unknown {
  if (value === undefined || value === null) return undefined;
  const values = Array.isArray(value) ? value : [value];
  const normalized: string[] = [];
  for (const entry of values) {
    if (typeof entry === 'string') normalized.push(entry);
    else if (typeof entry === 'number' || typeof entry === 'boolean') normalized.push(String(entry));
    else return undefined;
  }
  return normalized;
}

/** Boolean-flag query params: absent/other = closed, `1` = open. PARAM-060/061/066/071 etc. */
const flag = (defaultValue: '0' | '1' = '0') =>
  z.preprocess(toScalarString, z.enum(['0', '1']).catch(defaultValue).prefault(defaultValue));

/** Generic optional free-text param (id, label, url fragment, ...). */
const text = (defaultValue = '') =>
  z.preprocess(toScalarString, z.string().max(2048).catch(defaultValue).prefault(defaultValue));

/** Comma-repeatable multi-value param (`statuses`, `tags[]`). */
const list = () =>
  z.preprocess(toStringArray, z.array(z.string().max(256)).max(100).catch([]).prefault([]));

export const paramSchemas = {
  // ── agents detail/list (PARAM-001..020, PARAM-058) ──────────────────────
  destTab: text(),
  edited_participant_id: text(),
  isFromCreation: flag(),
  mcp: text(),
  newToolkitId: text(),
  return_url: text(),
  sort_by: text(),
  sort_order: z.enum(['asc', 'desc']).catch('desc').prefault('desc'),
  source_application_id: text(),
  viewMode: z.enum(['owner', 'public']).catch('owner').prefault('owner'),
  history_run_id: text(),

  // ── agents-hub / elitea-catalog (PARAM-021) ──────────────────────────────
  agentId: text(),
  /**
   * Which `/elitea-catalog` tab is showing. Not in P1's manifest: the
   * baseline grew this key after the manifest was extracted, when
   * `/agents-hub` became a redirect into the new two-tab catalogue
   * (`EliteaCatalog.jsx`'s `searchParams.get('tab')`).
   *
   * Declared as free text rather than `z.enum(['agents','skills'])` so an
   * unknown value round-trips to the page, which applies the baseline's own
   * "anything not `skills` means `agents`" rule
   * (`pages/elitea-catalog`'s `catalogTabFromSearch`). An enum with
   * `.catch()` would silently rewrite the URL instead.
   */
  tab: text(),

  // ── apps (PARAM-022/023) + every entity list page ────────────────────────
  //
  // ONE key, TWO vocabularies, both real. `pages/apps` writes `grid`/`list`
  // (its own catalogue toggle, PARAM-022/023); every OTHER list page — agents,
  // pipelines, skills, toolkits, MCPs, credentials — uses the baseline's
  // `ViewOptions` (`apps/elitea-ui/src/common/constants.js:318-321`:
  // `cards`/`table`), which is what `components/ViewToggle.jsx` writes and
  // `hooks/useIsTableView.js` reads. Both sets live in one enum because they
  // live on one query key; each reader only recognises its own two values and
  // falls back to its own default for the other pair
  // (`shared/ui/EntityCardList` treats anything that is not `table` as cards).
  view: z.enum(['grid', 'list', 'cards', 'table']).catch('grid').prefault('grid'),

  // ── artifacts (PARAM-024..027) ───────────────────────────────────────────
  bucket: text(),
  file: text(),
  folder: text(),
  shared_bucket: text(),

  // ── auth callback (PARAM-028) ────────────────────────────────────────────
  auth_state: text(),

  // ── chat (PARAM-029..036, PARAM-077/078) ─────────────────────────────────
  conversation: text(),
  message_id: text(),
  name: text(),
  shared_chat: flag(),
  /**
   * Replay the open conversation instead of continuing it.
   *
   * Not in P1's manifest: the baseline drives playback off a Redux flag on
   * the selected conversation, which this app has no equivalent of. The URL
   * is used instead so the surface has a real mount point — a playback view
   * reachable only from in-memory state is a view no link, reload or test
   * can reach.
   */
  playback: flag(),

  // ── credentials (PARAM-037..046) ─────────────────────────────────────────
  forceCustom: flag(),
  from: text(),
  prefill_id: text(),
  prefill_name: text(),
  section: text(),

  // ── indexes panel (PARAM-047, PARAM-070) ─────────────────────────────────
  index_name: text(),

  // ── mcp-auth-callback (PARAM-048..051) ───────────────────────────────────
  code: text(),
  error: text(),
  error_description: text(),
  state: text(),

  // ── settings (PARAM-060/061) ─────────────────────────────────────────────
  createSecret: flag(),
  inviteUsers: flag(),

  // ── shared/"any" scope (PARAM-062..087) ──────────────────────────────────
  author_id: text(),
  author_name: text(),
  create: flag(),
  project_id: text(),
  save_toolkit: flag(),
  statuses: list(),
  'tags[]': list(),
  toolkit_type: text(),
  tour: text(),
  page_size: z.coerce.number().int().positive().max(1000).catch(20).prefault(20),

  // ── skills (PARAM-088..093) ──────────────────────────────────────────────
  newSkillId: text(),

  // ── toolkits (PARAM-094..107, superset of credentials-style keys) ───────
  // (destTab, edited_participant_id, forceCustom, name, newToolkitId,
  //  return_url, source_application_id already declared above)

  // ── user-public (PARAM-108) ──────────────────────────────────────────────
  // (statuses already declared above)
} as const satisfies Record<string, z.ZodType>;

export type ParamKey = keyof typeof paramSchemas;

/** All distinct query-param keys the manifest identifies (45). */
export const PARAM_KEYS = Object.keys(paramSchemas) as readonly ParamKey[];

/**
 * Build a partial zod object schema from a subset of the registry, for a
 * route's `validateSearch`. Every field keeps its own explicit default, so
 * a cold load with none of the keys present still produces a fully-typed,
 * defaulted search object (TanStack Router's `validateSearch` contract).
 */
export function pickParams<K extends ParamKey>(...keys: readonly K[]) {
  const shape = {} as { [P in K]: (typeof paramSchemas)[P] };
  for (const key of keys) {
    shape[key] = paramSchemas[key];
  }
  return z.object(shape);
}

/**
 * The value each schema gives an ABSENT key. `__root.tsx` passes this map to
 * `stripSearchParams`, which deletes every key whose value still equals it.
 *
 * DEFECT this exists to correct. `validateSearch` returns EVERY declared key
 * on every call, because every schema above ends in `.prefault(default)`.
 * TanStack Router runs `validateSearch` inside `buildLocation` with
 * `_includeValidateSearch: true` (`router-core/dist/esm/router.js`, and
 * `buildAndCommitLocation` sets that flag). The full defaulted object was
 * therefore serialised back into the address bar on EVERY navigation. One click on an
 * in-app link to `/chat` produced a 359-character URL carrying 29 empty or
 * defaulted parameters. `JSON.stringify` also re-quoted the flag values, so
 * `0` reached the address bar as `%220%22`.
 *
 * The map is COMPUTED FROM THE SCHEMAS, never written out by hand. A
 * hand-written default that disagreed with a schema's `prefault` would strip
 * nothing, silently, and the next reader of this file could not see it. Each
 * schema is parsed with `undefined`, which is exactly the "key is absent"
 * input `validateSearch` gives it, so the two can never drift.
 *
 * The parsed search object still carries the defaults, which is what keeps
 * `useSearch()` readers working. Only the emitted URL loses them.
 */
export const PARAM_DEFAULTS: Readonly<Record<ParamKey, unknown>> = Object.freeze(
  Object.fromEntries(
    PARAM_KEYS.map((key) => [key, paramSchemas[key].parse(undefined)] as const),
  ),
) as Readonly<Record<ParamKey, unknown>>;
