// Offline chat search (src/db/search.ts) is an FTS5 index inside the SQLCipher offline store.
// Both have to be compiled into the native SQLite the app links: expo-sqlite builds its vendored
// SQLCipher amalgamation with the flags below. This pins those facts, so an SDK bump that
// drops FTS5 from the SQLCipher build (or a config change that turns it off) fails here and not
// as a store that cannot migrate on a phone. Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { describe, it } from 'node:test';

const root = new URL('..', import.meta.url);
const read = (path) => readFileSync(new URL(path, root), 'utf8');
const pkg = 'node_modules/expo-sqlite/';

describe('expo-sqlite: SQLCipher with FTS5', () => {
  it('the app asks for SQLCipher and leaves FTS on', () => {
    const config = read('app.config.ts');
    assert.match(config, /\['expo-sqlite', \{ useSQLCipher: true, enableFTS: true \}\]/);
    assert.doesNotMatch(config, /enableFTS:\s*false/);
  });

  it('iOS: the pod compiles the SQLCipher amalgamation with FTS5 unless enableFTS is false', () => {
    const podspec = read(`${pkg}ios/ExpoSQLite.podspec`);
    assert.match(podspec, /unless podfile_properties\['expo\.sqlite\.enableFTS'\] == 'false'\s*\n\s*sqlite_cflags << '[^']*-DSQLITE_ENABLE_FTS5=1/);
    assert.match(podspec, /vendor_src = useSQLCipher \? 'sqlcipher' : 'sqlite3'/);
    assert.match(podspec, /-DSQLITE_HAS_CODEC=1/);
  });

  it('Android: the CMake build gets FTS5 next to the SQLCipher codec', () => {
    const gradle = read(`${pkg}android/build.gradle`);
    assert.match(gradle, /if \(findProperty\('expo\.sqlite\.enableFTS'\) != 'false'\) \{\s*\n\s*buildFlags <<= '[^']*-DSQLITE_ENABLE_FTS5=1/);
    assert.match(gradle, /SQLITE3_SRC_DIR = new File\("\$\{projectDir\}\/\.\.\/vendor\/sqlcipher"\)/);
  });

  it('the vendored SQLCipher amalgamation carries the FTS5 module (SQLite 3.53)', () => {
    const amalgamation = read(`${pkg}vendor/sqlcipher/sqlite3.c`);
    assert.ok(amalgamation.includes('sqlite3Fts5Init'), 'no FTS5 source in the SQLCipher amalgamation');
    assert.match(amalgamation, /#define SQLITE_VERSION\s+"3\.5[3-9]\./);
  });
});
