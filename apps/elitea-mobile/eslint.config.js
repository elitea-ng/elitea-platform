// https://docs.expo.dev/guides/using-eslint/
const { defineConfig } = require('eslint/config');
const expoConfig = require('eslint-config-expo/flat');

/**
 * Modules that only one file may import. Each guard is a security boundary:
 * the facade is the single place the rule-protected capability is used.
 */
const RESTRICTED_MODULES = [
  {
    name: 'expo-secure-store',
    message:
      'Use src/security/secureStorage.ts: every key must be declared in its registry so endSession wipes it.',
    allowedIn: ['src/security/secureStorage.ts', 'src/security/secureStorage.test.ts'],
  },
  {
    name: 'expo-sqlite',
    message:
      'Use the offline store (src/db/offlineStore.ts): the connection must be keyed first and wiped with the session.',
    allowedIn: ['src/db/expoDriver.ts'],
  },
  {
    name: 'expo-network',
    message: 'Use src/lib/network.ts (one online state for react-query, the outbox and sync).',
    allowedIn: ['src/lib/network.ts'],
  },
  {
    name: 'expo-speech-recognition',
    message:
      'Use src/features/speech/dictation.ts: dictation must stay on the device (requiresOnDeviceRecognition), never Apple server recognition.',
    allowedIn: ['src/features/speech/dictation.ts', 'src/features/speech/dictation.test.tsx'],
  },
  {
    name: 'expo-speech',
    message: 'Use src/features/speech/readAloud.ts (one utterance at a time, stopped with the session).',
    allowedIn: ['src/features/speech/readAloud.ts', 'src/features/speech/readAloud.test.ts', 'src/features/chat/actions/messageActions.screen.test.tsx'],
  },
  {
    name: 'expo/fetch',
    message:
      'Only src/api/sse/transport.ts (streams) and src/api/noRedirectFetch.ts (token endpoints) use expo/fetch; REST goes through the API client in src/api/.',
    allowedIn: ['src/api/sse/transport.ts', 'src/api/noRedirectFetch.ts'],
  },
];

const SOURCE_FILES = ['**/*.{js,jsx,ts,tsx,mjs,cjs}'];

function restrictedImportsExcept(file) {
  const paths = RESTRICTED_MODULES.filter((m) => !m.allowedIn.includes(file)).map(
    ({ name, message }) => ({ name, message }),
  );
  return paths.length > 0 ? ['error', { paths }] : 'off';
}

module.exports = defineConfig([
  expoConfig,
  {
    // src/api/generated/ is produced by `npm run contract:gen` and drift-checked by `contract:check`.
    ignores: ['dist/*', 'src/api/generated/**', 'contract/.build/**'],
  },
  {
    files: SOURCE_FILES,
    rules: {
      'no-restricted-imports': restrictedImportsExcept(null),
    },
  },
  ...[...new Set(RESTRICTED_MODULES.flatMap((m) => m.allowedIn))].map((file) => ({
    files: [file],
    rules: { 'no-restricted-imports': restrictedImportsExcept(file) },
  })),
  {
    // Network access goes through src/api/ (origin guard, token handling, timeouts).
    files: ['src/**/*.{js,jsx,ts,tsx}'],
    ignores: ['src/api/**'],
    rules: {
      'no-restricted-globals': [
        'error',
        { name: 'fetch', message: 'Use the API client in src/api/ (origin guard, auth, timeouts).' },
      ],
    },
  },
  {
    // jest.mock factories must use require().
    files: ['**/*.test.{ts,tsx}', 'src/test/**'],
    rules: { '@typescript-eslint/no-require-imports': 'off' },
  },
  {
    // src/lib/log.ts is the only console writer (it redacts); scripts are CLI tools.
    files: ['src/**/*.{js,jsx,ts,tsx}'],
    rules: { 'no-console': 'error' },
  },
]);
