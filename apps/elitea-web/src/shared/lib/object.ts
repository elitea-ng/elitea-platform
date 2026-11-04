/**
 * Object helpers ported from apps/elitea-ui/src/common/utils.jsx (unit S3,
 * spec §9.3).
 */

/** Recursive structural type for the plain-data values these helpers accept. */
export type PlainData =
  | string
  | number
  | boolean
  | null
  | undefined
  | PlainData[]
  | { [key: string]: PlainData };

/**
 * Recursive clone of plain data (objects/arrays/primitives).
 *
 * Preserved quirk (N4, old-app `utils.jsx:761-780`): dispatch is by
 * `typeof obj === 'object'`, not by constructor — so a `Date` (typeof
 * `'object'`) is NOT cloned as a `Date`. It is walked via `Object.keys`,
 * which enumerates zero own-enumerable properties on a `Date` instance, so
 * the "clone" of a `Date` is `{}`, silently losing the value. Functions
 * (typeof `'function'`) fall through to the "anything else" branch and are
 * returned BY REFERENCE, not cloned. Both are ported as observed rather
 * than fixed — see the S3 report.
 */
export function deepCloneObject<T extends PlainData>(obj: T): T {
  // null, 0, false, '', undefined, NaN — every falsy value short-circuits.
  if (!obj) {
    return obj;
  }
  if (Array.isArray(obj)) {
    return obj.map((val) => deepCloneObject(val)) as T;
  }
  if (typeof obj === 'object') {
    const result: Record<string, PlainData> = {};
    for (const key of Object.keys(obj)) {
      result[key] = deepCloneObject((obj as Record<string, PlainData>)[key]);
    }
    return result as T;
  }
  return obj;
}

function isPlainObjectValue(value: PlainData): value is Record<string, PlainData> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

/** Leaf-assignment when `path` already has a value at this segment. */
function assignAtExistingLeaf(existing: PlainData, value: PlainData, replace: boolean | undefined): PlainData {
  if (isPlainObjectValue(existing) && isPlainObjectValue(value) && !replace) {
    return { ...existing, ...value };
  }
  if (Array.isArray(value)) {
    return [...value];
  }
  return value;
}

/** Leaf-assignment when `path` has no value yet at this segment. */
function assignAtNewLeaf(value: PlainData): PlainData {
  if (isPlainObjectValue(value)) {
    return { ...value };
  }
  if (Array.isArray(value)) {
    return [...value];
  }
  return value;
}

/**
 * Immutably sets `object[path]` (dot-separated) to `value`, deep-cloning
 * `object` first. When the existing value and the new value are both plain
 * objects (non-null, non-array) and `replace` is falsy, they are shallow
 * MERGED rather than replaced; arrays are always replaced wholesale (spread
 * into a new array, never merged by index).
 */
export function updateObjectByPath<T extends Record<string, PlainData>>(
  object: T,
  path: string,
  value: PlainData,
  replace?: boolean,
): T {
  const pathParts = path.split('.');
  const clone = deepCloneObject(object) as Record<string, PlainData>;
  let cursor: Record<string, PlainData> = clone;

  pathParts.forEach((part, index) => {
    const isLast = index === pathParts.length - 1;
    const existing = cursor[part];

    if (existing !== undefined) {
      if (!isLast) {
        cursor = existing as Record<string, PlainData>;
        return;
      }
      cursor[part] = assignAtExistingLeaf(existing, value, replace);
      return;
    }

    if (!isLast) {
      const next: Record<string, PlainData> = {};
      cursor[part] = next;
      cursor = next;
      return;
    }
    cursor[part] = assignAtNewLeaf(value);
  });

  return clone as T;
}

/** `true` for exactly `null` or `undefined` (not `0`, `''`, `false`, `NaN`). */
export function isNullOrUndefined(value: unknown): value is null | undefined {
  return value === null || value === undefined;
}

/** `true` for any non-null object (arrays included): a value whose fields can be read. */
export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

/** The trimmed string, or `undefined` for a non-string or a blank one. */
export function nonBlankText(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim() !== '' ? value.trim() : undefined;
}
