/**
 * jest setup: jest-expo's expo-constants mock has an empty `expoConfig`, so
 * modules that read the app identity (`src/config/identity.ts`) would throw.
 * Fill it from app.config.ts, the one place the identity is written down.
 */
import Constants from 'expo-constants';

const appConfig = require('../../app.config').default as (ctx: { config: Record<string, unknown> }) => {
  version?: string;
  extra?: Record<string, unknown>;
};

const resolved = appConfig({ config: {} });
const expoConfig = Constants.expoConfig as Record<string, unknown> | null;
if (expoConfig === null || typeof expoConfig !== 'object') {
  throw new Error('setupExpoConfig: expo-constants mock has no expoConfig object');
}
// expoConfig is a getter over a shared object: extend it in place.
Object.assign(expoConfig, { version: resolved.version, extra: resolved.extra });
