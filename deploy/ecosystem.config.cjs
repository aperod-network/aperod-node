/**
 * PM2 ecosystem file for aperod-api.
 *
 * Place this file at /opt/aperod/ecosystem.config.cjs on the server.
 *
 * Usage:
 *   # First-time start (after git pull + build):
 *   pm2 start /opt/aperod/ecosystem.config.cjs
 *
 *   # Subsequent updates — ALWAYS use the update script, never bare pm2 restart:
 *   sudo bash /opt/aperod/blockchain/deploy/update-api.sh
 *
 *   WHY: bare `pm2 restart aperod-api` replays the OLD compiled dist/index.mjs.
 *   New TypeScript source changes are silently ignored until a rebuild runs.
 *   The update script does: git pull → pnpm build → pm2 restart (in that order).
 *
 *   # OR if ecosystem file changed:
 *   pm2 startOrRestart /opt/aperod/ecosystem.config.cjs
 *
 * IMPORTANT: `pm2 delete + pm2 start` loses env vars — always use
 * `pm2 restart` or `pm2 startOrRestart` for updates.
 */

"use strict";

module.exports = {
  apps: [
    {
      name: "aperod-api",
      script: "/opt/aperod/artifacts/api-server/dist/index.mjs",
      node_args: "--enable-source-maps",
      // Load all env vars from the .env file at project root.
      // Adjust path if your .env lives elsewhere.
      env_file: "/opt/aperod/.env",
      // Disable file-watch mode — building to dist/ should NOT auto-restart
      // mid-build (each file write would trigger a partial restart).
      watch: false,
      // Restart on crash, but cap restarts to avoid infinite boot loops.
      autorestart: true,
      max_restarts: 10,
      restart_delay: 2000,
      // Log paths (PM2 default: ~/.pm2/logs/).
      out_file: "/root/.pm2/logs/aperod-api-out.log",
      error_file: "/root/.pm2/logs/aperod-api-error.log",
      merge_logs: true,
    },
  ],
};
