// Metro configuration (Expo SDK 58: https://docs.expo.dev/versions/v58.0.0/config/metro/).
//
// The app imports `@elitea/client-core` (libs/ts/client-core): TypeScript source
// outside the app root, shared with the web and desktop clients. The import
// resolves through the tsconfig `paths` alias (`experiments.tsconfigPaths`, on by
// default), and on-demand filesystem access lets Metro read the file. Adding the
// package to `watchFolders` gives it file-watcher coverage, so an edit there
// reloads the dev server like an edit in `src/`.
//
// The package has no node_modules of its own (no dependencies). A bare import
// the transform adds to its files (a Babel runtime helper) must resolve from
// this app's node_modules, as the jest `moduleDirectories` entry does for tests.
const path = require('node:path');

const { getDefaultConfig } = require('expo/metro-config');

const config = getDefaultConfig(__dirname);

config.watchFolders = [...(config.watchFolders ?? []), path.resolve(__dirname, '../../libs/ts/client-core')];
config.resolver.nodeModulesPaths = [...(config.resolver.nodeModulesPaths ?? []), path.resolve(__dirname, 'node_modules')];

module.exports = config;
