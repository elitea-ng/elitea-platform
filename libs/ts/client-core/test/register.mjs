/**
 * Lets `node --test` run the package's TypeScript source as the apps' bundlers
 * see it: Node strips the types (Node >= 23.6), and this hook resolves an
 * extensionless relative import (`./sseParser`) to its `.ts` file. The
 * source keeps extensionless imports because the apps' tsconfigs do not set
 * `allowImportingTsExtensions`.
 */
import { existsSync } from 'node:fs';
import { registerHooks } from 'node:module';
import { fileURLToPath } from 'node:url';

registerHooks({
  resolve(specifier, context, nextResolve) {
    if ((specifier.startsWith('./') || specifier.startsWith('../')) && !/\.[cm]?[jt]s$|\.json$|\.svg$/.test(specifier) && context.parentURL) {
      for (const candidate of [`${specifier}.ts`, `${specifier}/index.ts`]) {
        const url = new URL(candidate, context.parentURL);
        if (existsSync(fileURLToPath(url))) return nextResolve(url.href, context);
      }
    }
    return nextResolve(specifier, context);
  },
});
