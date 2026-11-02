/**
 * The version of this web build: the release the image was built for.
 *
 * The Containerfile turns publish.yml's `VERSION` build arg into
 * `VITE_APP_VERSION`, which Vite inlines at build time. A local build, a CI
 * build without the arg, and the unit tests have none, and read "dev".
 */
export function appVersion(): string {
  const raw: unknown = import.meta.env.VITE_APP_VERSION;
  return typeof raw === 'string' && raw.trim() !== '' ? raw.trim() : 'dev';
}
