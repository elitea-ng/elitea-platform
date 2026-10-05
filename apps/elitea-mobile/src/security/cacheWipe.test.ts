import { wipeCachesDirectory, type CacheDirectory } from '@/security/cacheWipe';

function dir(names: string[], failing: string[] = []) {
  const left = new Set(names);
  const directory: CacheDirectory = {
    exists: true,
    list: () =>
      names.map((name) => ({
        delete: () => {
          if (failing.includes(name)) throw new Error(`cannot delete ${name}`);
          left.delete(name);
        },
      })),
  };
  return { directory, left };
}

describe('wipeCachesDirectory (DEF-R2)', () => {
  it('deletes every entry, the NSURLCache folder included', () => {
    const { directory, left } = dir(['ca.zefir.agent', 'fsCachedData', 'expo-image']);
    wipeCachesDirectory(directory, 'ios');
    expect([...left]).toEqual([]);
  });

  it('tries every entry and then reports the first failure', () => {
    const { directory, left } = dir(['a', 'b', 'c'], ['a']);
    expect(() => wipeCachesDirectory(directory, 'android')).toThrow('cannot delete a');
    expect([...left]).toEqual(['a']);
  });

  it('does nothing on web and for a missing directory', () => {
    const { directory, left } = dir(['x']);
    wipeCachesDirectory(directory, 'web');
    wipeCachesDirectory({ exists: false, list: () => { throw new Error('unreachable'); } }, 'ios');
    expect([...left]).toEqual(['x']);
  });
});
