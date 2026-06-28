import { Router } from "express";
import { adminGuard } from "../../middlewares/admin-guard";
import { registerValidatorsGetter, getValidatorHistory, getAllValidatorHistory } from "../../lib/shared-state";
import { db, validatorsTable, mintLogTable } from "@workspace/db";
import { eq } from "drizzle-orm";
import { randomBytes } from "node:crypto";
import { logger } from "../../lib/logger";
import { adminNotifier } from "../../lib/admin-notifier";
import { goNodeRest } from "../../lib/go-node-client";
import { getAllByAddress } from "../../lib/tx-registry.js";

const router = Router();

// In-memory cache — populated from DB on startup, kept in sync on mutations
type ValidatorRow = {
  id: string;
  pubKey: string;
  alias: string;
  endpoint: string;
  address: string | null;
  online: boolean;
  forceActive: boolean;
  addedAt: number;
  lastSeen: number | null;
};

let _cache: ValidatorRow[] = [];

function dbRowToValidator(v: { id: string; pub_key: string; alias: string; endpoint: string; address: string | null | undefined; online: boolean; force_active: boolean; added_at: number; last_seen: number | null | undefined }): ValidatorRow {
  return { id: v.id, pubKey: v.pub_key, alias: v.alias, endpoint: v.endpoint, address: v.address ?? null, online: v.online, forceActive: v.force_active, addedAt: v.added_at, lastSeen: v.last_seen ?? null };
}

async function refreshCache(): Promise<void> {
  try {
    const rows = await db.select().from(validatorsTable).orderBy(validatorsTable.added_at);
    _cache = rows.map(dbRowToValidator);
  } catch (err) {
    logger.error({ err }, "Failed to refresh validators cache from DB");
  }
}

// Load on startup
void refreshCache();

// Register so telegram-notifier / system monitor can read validators synchronously
registerValidatorsGetter(() =>
  _cache.map((v) => ({ id: v.id, alias: v.alias, online: v.online, lastSeen: v.lastSeen })),
);

router.get("/validators", adminGuard, async (_req, res) => {
  await refreshCache();
  res.json({ validators: _cache, total: _cache.length });
});

router.post("/validators", adminGuard, async (req, res) => {
  const { pubKey, alias, endpoint } = req.body as { pubKey?: string; alias?: string; endpoint?: string };

  if (!pubKey || pubKey.length < 32 || pubKey.length > 128) {
    res.status(400).json({ error: "Bad Request", message: "pubKey must be 32–128 hex chars" });
    return;
  }

  const existing = await db.select().from(validatorsTable).where(eq(validatorsTable.pub_key, pubKey));
  if (existing.length > 0) {
    res.status(409).json({ error: "Conflict", message: "Validator with this pubKey already exists" });
    return;
  }

  const id = `v${randomBytes(4).toString("hex")}`;
  const aliasVal = alias?.slice(0, 64) ?? `validator-${id}`;
  const [inserted] = await db.insert(validatorsTable).values({
    id,
    pub_key: pubKey,
    alias: aliasVal,
    endpoint: endpoint?.slice(0, 256) ?? "",
    online: false,
    added_at: Date.now(),
    last_seen: null,
  }).returning();

  await refreshCache();
  const validator = dbRowToValidator(inserted!);
  adminNotifier.notifyValidatorAdded(validator.alias, validator.pubKey, validator.endpoint);
  res.status(201).json({ message: "Validator added", validator });
});

router.get("/validators/:alias/history", adminGuard, (req, res) => {
  const alias = String(req.params["alias"] ?? "");
  const history = getValidatorHistory(alias);
  res.json({ alias, history });
});

router.get("/validators/history", adminGuard, (_req, res) => {
  const history = getAllValidatorHistory();
  res.json({ history });
});

// ── GET /validators/:id/balance ─ fetch stake balance from Go node ──────────
router.get("/validators/:id/balance", adminGuard, async (req, res) => {
  const id = String(req.params["id"] ?? "");
  const rows = await db.select().from(validatorsTable).where(eq(validatorsTable.id, id));
  if (rows.length === 0) {
    res.status(404).json({ error: "Not Found" });
    return;
  }
  const v = rows[0]!;
  if (!v.address) {
    res.json({ address: null, balance_napr: 0, balance_apr: 0, min_stake_apr: 100_000, threshold_met: false });
    return;
  }
  let balanceNAPR = 0;
  let nodeReachable = false;

  // 1. Try Go node REST (not JSON-RPC — Go node only exposes REST at /api/v1/*)
  try {
    interface GoTx { amount?: number; direction?: "in" | "out"; confirmed?: boolean; }
    const data = await goNodeRest<{ transactions?: GoTx[] }>(`/api/v1/address/${v.address}/transactions`);
    const txs = data.transactions ?? [];
    for (const tx of txs) {
      if (tx.direction === "in" && tx.confirmed && typeof tx.amount === "number") {
        balanceNAPR += tx.amount;
      }
    }
    nodeReachable = true;
  } catch { /* node REST unavailable, fall through */ }

  // 2. Fallback: DB mint_log (authoritative for all minted amounts)
  try {
    const mintRows = await db.select().from(mintLogTable).where(eq(mintLogTable.to_address, v.address));
    const mintNAPR = mintRows.reduce((s, r) => s + r.amount * 1e8, 0);
    if (mintNAPR > 0 || !nodeReachable) {
      if (mintNAPR > balanceNAPR) balanceNAPR = mintNAPR;
      nodeReachable = true;
    }
  } catch (err) {
    logger.warn({ err, address: v.address }, "mint_log lookup failed for validator balance");
  }

  // 3. Add in-session pending txs from memory registry (e.g. wallet sends to this address)
  const registryTxs = getAllByAddress(v.address);
  let pendingInAPR = 0;
  let pendingOutAPR = 0;
  for (const reg of registryTxs) {
    if (reg.to_address === v.address && reg.from_address !== v.address) {
      pendingInAPR += reg.amount_apr;
    }
    if (reg.from_address === v.address) {
      pendingOutAPR += reg.amount_apr + reg.fee_apr;
    }
  }
  const adjustedNAPR = Math.max(0, balanceNAPR + Math.round((pendingInAPR - pendingOutAPR) * 1e8));
  if (pendingInAPR > 0 || pendingOutAPR > 0) nodeReachable = true;

  const balanceAPR = adjustedNAPR / 1e8;
  res.json({
    address: v.address,
    balance_napr: adjustedNAPR,
    balance_apr: balanceAPR,
    min_stake_apr: 100_000,
    threshold_met: balanceAPR >= 100_000,
    node_reachable: nodeReachable,
  });
});

router.patch("/validators/:id", adminGuard, async (req, res) => {
  const id = String(req.params["id"] ?? "");
  const body = req.body as { online?: boolean; address?: string | null };

  const rows = await db.select().from(validatorsTable).where(eq(validatorsTable.id, id));
  if (rows.length === 0) {
    res.status(404).json({ error: "Not Found", message: "Validator not found" });
    return;
  }

  // Update online status
  if (typeof body.online === "boolean") {
    const now = Date.now();
    await db
      .update(validatorsTable)
      .set({ online: body.online, force_active: body.online, last_seen: body.online ? now : rows[0]!.last_seen })
      .where(eq(validatorsTable.id, id));
    await refreshCache();
    logger.info({ id, online: body.online }, "Validator online status updated by admin");
    res.json({ message: "Validator updated", id, online: body.online });
    return;
  }

  // Update wallet address
  if ("address" in body) {
    const address = body.address ? String(body.address).trim() : null;
    if (address !== null && address.length < 40) {
      res.status(400).json({ error: "Bad Request", message: "APR address must be at least 40 characters" });
      return;
    }
    await db
      .update(validatorsTable)
      .set({ address })
      .where(eq(validatorsTable.id, id));
    await refreshCache();
    logger.info({ id, address }, "Validator wallet address updated by admin");
    res.json({ message: "Validator address updated", id, address });
    return;
  }

  res.status(400).json({ error: "Bad Request", message: "Provide online (boolean) or address (string|null)" });
});

router.delete("/validators/:id", adminGuard, async (req, res) => {
  const id = String(req.params["id"] ?? "");
  const rows = await db.select().from(validatorsTable).where(eq(validatorsTable.id, id));
  if (rows.length === 0) {
    res.status(404).json({ error: "Not Found", message: "Validator not found" });
    return;
  }
  const target = rows[0]!;

  await db.delete(validatorsTable).where(eq(validatorsTable.id, id));
  await refreshCache();
  res.json({ message: "Validator removed", validator: { id: target.id, pubKey: target.pub_key, alias: target.alias } });
});

export default router;
