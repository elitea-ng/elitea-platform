/** Fixed Parallel branch ids and Map worker ids have separate meanings. */
function referenceRecord(value: unknown): Readonly<Record<string, unknown>> | undefined {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return undefined;
  return value as Readonly<Record<string, unknown>>;
}
export function fixedParallelWorkerReferences(value: unknown): readonly string[] {
  if (!Array.isArray(value)) return [];
  return value.flatMap((branch: unknown) => {
    const reference = referenceRecord(branch)?.['node'];
    return typeof reference === 'string' ? [reference] : [];
  });
}
export function graphExtensionReferenceUpdate(record: Readonly<Record<string, unknown>>, name: string, next: string): Readonly<Record<string, unknown>> {
  if (record['type'] === 'map' && record['worker'] === name) return { worker: next };
  if (record['type'] !== 'parallel' || !Array.isArray(record['branches'])) return {};
  let changed = false;
  const branches = record['branches'].map((branch: unknown) => {
    const source = referenceRecord(branch);
    if (!source || source['node'] !== name) return branch;
    changed = true;
    return { ...source, node: next };
  });
  return changed ? { branches } : {};
}
