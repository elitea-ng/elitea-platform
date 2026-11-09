/**
 * The web push service worker's entry: bundled by the `elitea-push-worker`
 * Vite plugin into the classic script `push-sw.js` at the app's base path.
 */
import { installPushWorker, type PushWorkerScope } from './pushWorker';

// The worker's global is a ServiceWorkerGlobalScope; the app's tsconfig types
// `self` as a Window (DOM lib), so it is narrowed to the part the worker uses.
// The build's product name (the default pack's shortName), which the
// elitea-push-worker plugin defines (vite.config.ts).
const product: unknown = import.meta.env.VITE_PRODUCT_NAME;
installPushWorker(
  self as unknown as PushWorkerScope,
  typeof product === 'string' && product !== '' ? product : 'Assistant',
);
