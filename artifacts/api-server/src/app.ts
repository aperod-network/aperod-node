import express, { type Express } from "express";
import cors from "cors";
import cookieParser from "cookie-parser";
import helmet from "helmet";
import pinoHttp from "pino-http";
import os from "node:os";
import router from "./routes";
import adminAuthRouter from "./routes/admin/auth";
import adminSystemRouter from "./routes/admin/system";
import adminFeesRouter from "./routes/admin/fees";
import adminEmissionRouter from "./routes/admin/emission";
import adminValidatorsRouter from "./routes/admin/validators";
import adminApiKeysRouter from "./routes/admin/api-keys";
import adminSecurityRouter from "./routes/admin/security";
import adminInfraRouter from "./routes/admin/infrastructure";
import adminExchangeConfigRouter from "./routes/admin/exchange-config";
import adminNowPaymentsRouter from "./routes/admin/nowpayments";
import adminTelegramConfigRouter from "./routes/admin/telegram-config";
import { adminRouter as adminPanelSecurityRouter, publicRouter as panelSecurityPublicRouter } from "./routes/admin/panel-security";
import adminAccountsRouter from "./routes/admin/accounts";
import pricesRouter from "./routes/prices";
import exchangeDocsRouter from "./routes/exchange-docs";
import validatorsApplyRouter from "./routes/validators-apply";
import nowPaymentsRouter from "./routes/nowpayments";
import { logger } from "./lib/logger";
import { rateLimiter } from "./middlewares/rate-limit";
import { apiKeyGuard } from "./middlewares/api-key";
import { securityHeaders } from "./middlewares/security-headers";
import { adminRateLimit } from "./middlewares/admin-rate-limit";
import { adminIpWhitelist } from "./middlewares/admin-ip-whitelist";
import { banEnforcement } from "./middlewares/ban-enforcement";
import { startPriceService } from "./lib/price-service";
import { startValidatorEpochJob } from "./lib/validator-epoch";
import { startValidatorHealthCheck } from "./lib/validator-health";
import { adminNotifier, loadNotificationLog, startNotificationLogCleanup } from "./lib/admin-notifier";
import { getValidators, recordValidatorTransition } from "./lib/shared-state";
import { onPayment } from "./lib/telegram-notifier";
import { shouldAlert, COOLDOWNS } from "./lib/alert-cooldown";
import { loadTelegramConfig } from "./lib/telegram-config-store";
import { loadExchangeConfig } from "./lib/exchange-config-store";
import { initAdminAuth } from "./routes/admin/auth";
import { loadFeeConfig } from "./lib/fee-config-store";
import { loadEmissionConfig } from "./lib/emission-config-store";
import { loadFaucetConfig } from "./lib/faucet-config-store";
import { STILL_OFFLINE_THRESHOLD_MS, runtimeInactivity } from "./lib/runtime-config";
import { deactivateInactiveAccounts } from "./lib/admin-accounts";
import { loadInactivityConfig } from "./lib/inactivity-config-store";
import { loadBruteForceConfig } from "./lib/brute-force-config-store";
import { loadBansFromDb } from "./lib/shared-state";
import { startBanExpiryJob } from "./lib/ban-expiry-job";

const app: Express = express();

const ALLOWED_ORIGINS = (process.env["CORS_ORIGINS"] ?? "")
  .split(",")
  .map((o) => o.trim())
  .filter(Boolean);

app.use(
  pinoHttp({
    logger,
    serializers: {
      req(req) {
        return {
          id: req.id,
          method: req.method,
          url: req.url?.split("?")[0],
        };
      },
      res(res) {
        return {
          statusCode: res.statusCode,
        };
      },
    },
  }),
);

app.use(
  cors({
    origin(origin, cb) {
      if (!origin) return cb(null, true);
      if (ALLOWED_ORIGINS.length === 0) return cb(null, true);
      if (ALLOWED_ORIGINS.includes(origin)) return cb(null, true);
      return cb(new Error(`CORS: origin '${origin}' not allowed`), false);
    },
    methods: ["GET", "POST", "PATCH", "DELETE", "OPTIONS"],
    allowedHeaders: ["Content-Type", "X-Api-Key"],
    credentials: true,
    maxAge: 86400,
  }),
);

app.use(helmet({ contentSecurityPolicy: false }));
app.use(securityHeaders);
app.use(cookieParser());
app.use(express.json({ limit: "512kb" }));
app.use(express.urlencoded({ extended: true, limit: "512kb" }));

// ─── IP ban enforcement (runs before all route handlers) ──────────────────────
app.use(banEnforcement);

// Admin routes (IP whitelisted, separate rate limit)
app.use("/api/admin", adminIpWhitelist);
app.use("/api/admin/auth", adminRateLimit, adminAuthRouter);
app.use("/api/admin", adminSystemRouter);
app.use("/api/admin", adminFeesRouter);
app.use("/api/admin", adminEmissionRouter);
app.use("/api/admin", adminValidatorsRouter);
app.use("/api/admin", adminApiKeysRouter);
app.use("/api/admin", adminSecurityRouter);
app.use("/api/admin", adminInfraRouter);
app.use("/api/admin", adminExchangeConfigRouter);
app.use("/api/admin", adminNowPaymentsRouter);
app.use("/api/admin", adminTelegramConfigRouter);
app.use("/api/admin", adminPanelSecurityRouter);
app.use("/api/admin", adminAccountsRouter);

// Public endpoints (no API key required) — mounted before rateLimiter + apiKeyGuard
app.use("/api", panelSecurityPublicRouter);
app.use("/api", pricesRouter);
app.use("/api", exchangeDocsRouter);
app.use("/api", validatorsApplyRouter);
app.use("/api", nowPaymentsRouter);

// Public + key-protected API routes
app.use("/api", rateLimiter);
app.use("/api", apiKeyGuard);
app.use("/api", router);

// 500-error alerting
app.use((err: Error, req: express.Request, res: express.Response, _next: express.NextFunction) => {
  logger.error({ err }, "Unhandled server error");
  adminNotifier.notifyApiError(req.path, 500, err.message ?? "Unhandled error");
  res.status(500).json({ error: "Internal Server Error" });
});

// ─── Background services ──────────────────────────────────────────────────────

// Seed runtime config from DB (non-blocking; falls back to env defaults on error)
void initAdminAuth();
void loadTelegramConfig();
void loadExchangeConfig();
void loadFeeConfig();
void loadEmissionConfig();
void loadFaucetConfig();
void loadNotificationLog();
void loadInactivityConfig();
void loadBruteForceConfig();
void loadBansFromDb();

startNotificationLogCleanup();
startPriceService();
startSystemMonitor();
startInactivityJob();
startBanExpiryJob();
startValidatorEpochJob();
startValidatorHealthCheck();

// Register callback so simulated blockchain payments also trigger admin alert
onPayment((address, amount, txHash) => {
  adminNotifier.notifyPayment(address, amount, txHash);
});

function startSystemMonitor(): void {
  let lastCpuInfo = os.cpus();

  function getCpuPercent(): number {
    const current = os.cpus();
    let totalIdle = 0;
    let totalTick = 0;
    for (let i = 0; i < current.length; i++) {
      const cur = current[i]!;
      const prev = lastCpuInfo[i];
      if (!prev) continue;
      const idleDiff = cur.times.idle - prev.times.idle;
      const curTotal = Object.values(cur.times).reduce((a, b) => a + b, 0);
      const prevTotal = Object.values(prev.times).reduce((a, b) => a + b, 0);
      totalIdle += idleDiff;
      totalTick += curTotal - prevTotal;
    }
    lastCpuInfo = current;
    return totalTick > 0 ? Math.round((1 - totalIdle / totalTick) * 100) : 0;
  }

  function getRamPercent(): number {
    return Math.round((1 - os.freemem() / os.totalmem()) * 100);
  }

  const CPU_THRESHOLD = 80;
  const RAM_THRESHOLD = 85;

  /**
   * Tracks the last known online state per validator alias so we only alert
   * on transitions (online→offline or offline→online), not on every tick.
   * Undefined means the validator has not been seen yet this session.
   */
  const validatorOnlineState = new Map<string, boolean>();

  /**
   * Unix ms timestamp recording when each validator first transitioned to
   * offline in the current outage. Cleared when the validator recovers.
   * Used to fire the "still offline" follow-up reminder.
   */
  const validatorOfflineSince = new Map<string, number>();

  setInterval(() => {
    const cpuPct = getCpuPercent();
    const ramPct = getRamPercent();

    if ((cpuPct > CPU_THRESHOLD || ramPct > RAM_THRESHOLD) && shouldAlert("system", COOLDOWNS.SYSTEM_ALERT_MS)) {
      logger.warn({ cpuPct, ramPct }, "System alert threshold exceeded");
      adminNotifier.notifySystemAlert(cpuPct, ramPct, Math.floor(process.uptime()));
    }

    // Transition-based node status alerting — only fire on state changes
    const validators = getValidators();
    for (const v of validators) {
      const prev = validatorOnlineState.get(v.alias);

      if (prev === undefined) {
        // First time we've seen this validator — record state without alerting
        validatorOnlineState.set(v.alias, v.online);
        if (!v.online) {
          logger.warn({ alias: v.alias }, "Validator already offline at monitor start");
          // Start tracking the outage duration even for validators that were
          // already offline when the monitor started, so the reminder still fires.
          validatorOfflineSince.set(v.alias, Date.now());
        }
        continue;
      }

      if (prev && !v.online) {
        // Transition: online → offline
        logger.warn({ alias: v.alias }, "Validator went offline");
        adminNotifier.notifyNodeOffline(v.alias, v.lastSeen);
        recordValidatorTransition(v.alias, "offline");
        validatorOnlineState.set(v.alias, false);
        validatorOfflineSince.set(v.alias, Date.now());
      } else if (!prev && v.online) {
        // Transition: offline → online (recovery)
        logger.info({ alias: v.alias }, "Validator came back online");
        adminNotifier.notifyNodeOnline(v.alias);
        recordValidatorTransition(v.alias, "online");
        validatorOnlineState.set(v.alias, true);
        validatorOfflineSince.delete(v.alias);
      } else if (!v.online) {
        // Still offline — fire a follow-up reminder if past the threshold.
        // The adminNotifier's per-alias cooldown (node_still_offline) prevents
        // it from spamming: it fires at most once per cooldown window.
        const offlineSince = validatorOfflineSince.get(v.alias);
        if (offlineSince !== undefined && Date.now() - offlineSince >= STILL_OFFLINE_THRESHOLD_MS) {
          logger.warn({ alias: v.alias, offlineSince }, "Validator still offline past threshold — sending reminder");
          adminNotifier.notifyNodeStillOffline(v.alias, offlineSince);
        }
      }
    }
  }, 60_000);

  logger.info({ cpu_threshold: CPU_THRESHOLD, ram_threshold: RAM_THRESHOLD }, "System monitor started");
}

/**
 * Periodically deactivates admin accounts that have not logged in within
 * the configured inactivity threshold. Runs every 6 hours.
 */
function startInactivityJob(): void {
  const RUN_INTERVAL_MS = 6 * 60 * 60 * 1000; // 6 hours

  async function runDeactivation(): Promise<void> {
    const { thresholdDays } = runtimeInactivity;
    if (thresholdDays <= 0) return; // disabled

    try {
      const deactivated = await deactivateInactiveAccounts(thresholdDays);
      if (deactivated.length > 0) {
        logger.warn({ deactivated, thresholdDays }, "Inactivity job: accounts deactivated");
        adminNotifier.notifyAccountsDeactivated(deactivated, thresholdDays);
      } else {
        logger.info({ thresholdDays }, "Inactivity job: no accounts deactivated");
      }
    } catch (err) {
      logger.error({ err }, "Inactivity job: failed");
    }
  }

  // Run once after a short delay on startup, then every 6 hours
  setTimeout(() => { void runDeactivation(); }, 60_000);
  setInterval(() => { void runDeactivation(); }, RUN_INTERVAL_MS);

  logger.info("Account inactivity deactivation job started");
}

export default app;
