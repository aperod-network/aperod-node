/**
 * Per-validator TCP connectivity health check.
 *
 * Every CHECK_INTERVAL_MS the job attempts a TCP connection to each
 * validator's P2P port (parsed from the /ip4/…/tcp/… endpoint field).
 *
 * Success  → resets the fail counter, updates last_seen in DB.
 * Failure  → increments the fail counter. After OFFLINE_THRESHOLD consecutive
 *             failures the validator is marked online=false in DB.
 *
 * Re-activation is handled by the validator-epoch job once the node becomes
 * reachable again AND has sufficient stake.
 */

import net from "node:net";
import { db, validatorsTable } from "@workspace/db";
import { eq } from "drizzle-orm";
import { logger } from "./logger.js";
import { adminNotifier } from "./admin-notifier.js";
import { recordValidatorTransition } from "./shared-state.js";

const CHECK_INTERVAL_MS = 2 * 60 * 1_000; // 2 minutes between checks
const OFFLINE_THRESHOLD  = 3;              // consecutive failures before marking offline
const TCP_TIMEOUT_MS     = 5_000;          // 5 s per connection attempt

/** Consecutive TCP-failure counter per validator id. */
const failCount = new Map<string, number>();

/**
 * Parses a multiaddr like /ip4/1.2.3.4/tcp/30303 → { host, port }.
 * Returns null for unrecognised formats.
 */
function parseEndpoint(endpoint: string): { host: string; port: number } | null {
  const m = /\/ip4\/([\d.]+)\/tcp\/(\d+)/.exec(endpoint ?? "");
  if (!m) return null;
  return { host: m[1]!, port: Number(m[2]!) };
}

/** Tries to open a TCP connection; resolves true if the port accepts it. */
function tcpReachable(host: string, port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const sock = net.createConnection({ host, port, timeout: TCP_TIMEOUT_MS });
    sock.once("connect", () => { sock.destroy(); resolve(true); });
    sock.once("error",   () => { sock.destroy(); resolve(false); });
    sock.once("timeout", () => { sock.destroy(); resolve(false); });
  });
}

async function checkAll(): Promise<void> {
  let rows: (typeof validatorsTable.$inferSelect)[];
  try {
    rows = await db.select().from(validatorsTable);
  } catch (err) {
    logger.warn({ err }, "Validator health check: failed to load validators from DB");
    return;
  }

  for (const v of rows) {
    const addr = parseEndpoint(v.endpoint ?? "");
    if (!addr) continue; // no parseable endpoint — skip

    const reachable = await tcpReachable(addr.host, addr.port);
    const now = Date.now();

    if (reachable) {
      failCount.set(v.id, 0);
      // Update last_seen to reflect confirmed connectivity
      db.update(validatorsTable)
        .set({ last_seen: now })
        .where(eq(validatorsTable.id, v.id))
        .catch((err: unknown) =>
          logger.warn({ err, alias: v.alias }, "Health check: last_seen update failed"),
        );
      logger.debug({ alias: v.alias, host: addr.host, port: addr.port }, "Validator TCP reachable");
    } else {
      const prev = failCount.get(v.id) ?? 0;
      const next = prev + 1;
      failCount.set(v.id, next);
      logger.warn(
        { alias: v.alias, host: addr.host, port: addr.port, consecutiveFails: next, threshold: OFFLINE_THRESHOLD },
        "Validator TCP unreachable",
      );

      if (next >= OFFLINE_THRESHOLD && v.online) {
        try {
          await db.update(validatorsTable)
            .set({ online: false })
            .where(eq(validatorsTable.id, v.id));
          logger.warn({ alias: v.alias, consecutiveFails: next }, "Validator marked offline by health check (TCP unreachable)");
          adminNotifier.notifyNodeOffline(v.alias, v.last_seen ?? null);
          recordValidatorTransition(v.alias, "offline");
        } catch (err) {
          logger.error({ err, alias: v.alias }, "Health check: failed to mark validator offline in DB");
        }
      }
    }
  }
}

export function startValidatorHealthCheck(): void {
  // Delay the first run 15 s so the server finishes startup before pinging
  setTimeout(() => { void checkAll(); }, 15_000);
  setInterval(() => { void checkAll(); }, CHECK_INTERVAL_MS);
  logger.info(
    { intervalMs: CHECK_INTERVAL_MS, offlineThreshold: OFFLINE_THRESHOLD, tcpTimeoutMs: TCP_TIMEOUT_MS },
    "Validator TCP health check started",
  );
}
