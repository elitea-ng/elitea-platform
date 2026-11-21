#!/bin/sh
# Runtime config injection for the elitea-web SPA image (contracts C5/C7).
# Writes window.elitea_ui_config from environment variables at container start.
#
# Exactly the five honoured keys (contract C7). The two dev-mode auth keys
# from the old chain are intentionally not emitted (contract C7b / defect
# D10): a static bearer credential must never land in a world-readable
# config.js in a production image.
#
# The native E2E stack (deploy/native/e2e-standalone.mjs) runs this same
# script on the host, as the runner user: ELITEA_WEB_CONFIG_JS names where the
# file goes, and the arguments go to nginx (its prefix and config). The image
# sets neither, so it writes and runs exactly what it always did.
set -eu

cat > "${ELITEA_WEB_CONFIG_JS:-/usr/share/nginx/html/config.js}" <<EOF
window.elitea_ui_config = {
  vite_server_url: "${VITE_SERVER_URL:-}",
  vite_base_uri: "${VITE_BASE_URI:-/app/}",
  vite_socket_server: "${VITE_SOCKET_SERVER:-}",
  vite_socket_path: "${VITE_SOCKET_PATH:-}",
  vite_public_project_id: "${VITE_PUBLIC_PROJECT_ID:-}"
};
EOF

exec nginx -g "daemon off;" "$@"
