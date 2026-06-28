import { Router, type Request, type Response } from "express";
import { lookupByCode } from "../lib/tx-registry.js";
import { onNewBlock as telegramOnNewBlock } from "../lib/telegram-notifier.js";
import { runtimeEmission } from "../lib/runtime-config.js";
import { saveEmissionConfig } from "../lib/emission-config-store.js";
import { applyBlockReward } from "./admin/emission.js";
import { logger } from "../lib/logger.js";
import { goNodeRest, goNodeRpc } from "../lib/go-node-client.js";
import { db, mintLogTable } from "@workspace/db";
import { sql } from "drizzle-orm";

const router = Router();

// ─── SSE — live block feed ────────────────────────────────────────────────────

const sseClients = new Set<Response>();

let lastKnownHeight = -1;

// Poll Go node every 3 s; emit SSE events only when chain height grows.
setInterval(async () => {
  try {
    const stats = await goNodeRest<{ height: number }>("/api/v1/network/stats");
    const newHeight = Number(stats.height);

    if (lastKnownHeight === -1) {
      // First successful poll — sync height without emitting stale blocks.
      lastKnownHeight = newHeight;
      return;
    }

    if (newHeight > lastKnownHeight) {
      // Emit at most 10 new blocks per tick to avoid floods.
      const from = Math.max(lastKnownHeight + 1, newHeight - 9);
      for (let h = from; h <= newHeight; h++) {
        try {
          const block = await goNodeRest<Record<string, unknown>>(`/api/v1/blocks/${h}`);
          sseClients.forEach((res) => {
            try {
              res.write(
                `data: ${JSON.stringify({ topic: "new_block", data: block })}\n\n`,
              );
            } catch {
              sseClients.delete(res);
            }
          });

          telegramOnNewBlock(h).catch(() => void 0);

          const { actual, capped, newTotalSupply } = applyBlockReward(
            runtimeEmission.totalSupply,
            runtimeEmission.blockReward,
            runtimeEmission.maxSupply,
          );
          if (actual > 0) {
            runtimeEmission.totalSupply = newTotalSupply;
            saveEmissionConfig().catch((err: unknown) =>
              logger.error({ err }, "Failed to persist totalSupply after block reward"),
            );
          }
          if (capped) {
            logger.warn({ height: h }, "Block reward capped at maxSupply — supply limit reached");
          }
        } catch (err) {
          logger.warn({ err, height: h }, "Failed to fetch block details from Go node");
        }
      }
      lastKnownHeight = newHeight;
    }
  } catch (err) {
    logger.warn({ err }, "Go node poll failed — node may be offline");
  }
}, 3000);

// ─── GET /api/v1/events (SSE stream) ─────────────────────────────────────────

router.get("/v1/events", (req: Request, res: Response) => {
  if (sseClients.size >= 1000) {
    res.status(503).json({ error: "Too many SSE clients — server at capacity" });
    return;
  }

  res.set({
    "Content-Type": "text/event-stream",
    "Cache-Control": "no-cache",
    Connection: "keep-alive",
    "Access-Control-Allow-Origin": "*",
    "X-Accel-Buffering": "no",
  });
  res.flushHeaders();

  res.write(
    `data: ${JSON.stringify({ topic: "connected", data: { height: lastKnownHeight } })}\n\n`,
  );

  sseClients.add(res);

  const heartbeat = setInterval(() => {
    try {
      res.write(": heartbeat\n\n");
    } catch {
      clearInterval(heartbeat);
      sseClients.delete(res);
    }
  }, 25_000);

  req.on("close", () => {
    clearInterval(heartbeat);
    sseClients.delete(res);
  });
});

// ─── GET /api/v1/blocks ───────────────────────────────────────────────────────

router.get("/v1/blocks", async (req: Request, res: Response) => {
  const limit = Math.min(Number(req.query["limit"]) || 20, 100);
  const offset = Number(req.query["offset"]) || 0;
  try {
    const data = await goNodeRest<unknown>("/api/v1/blocks", { limit, offset });
    return res.json(data);
  } catch (err) {
    logger.error({ err }, "Failed to fetch blocks from Go node");
    return res.status(502).json({ error: "Blockchain node unavailable — try again later" });
  }
});

// ─── GET /api/v1/blocks/:id ───────────────────────────────────────────────────

router.get("/v1/blocks/:id", async (req: Request, res: Response) => {
  const id = String(req.params["id"] ?? "");
  const isHeight = /^\d+$/.test(id);
  const isHash = /^[0-9a-f]{64}$/i.test(id);
  if (!isHeight && !isHash) {
    return res.status(400).json({ error: "id must be a height (integer) or 64-hex-char hash" });
  }
  try {
    const data = await goNodeRest<unknown>(`/api/v1/blocks/${id}`);
    return res.json(data);
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    if (msg.includes("HTTP 404")) return res.status(404).json({ error: "block not found" });
    logger.error({ err, id }, "Failed to fetch block from Go node");
    return res.status(502).json({ error: "Blockchain node unavailable — try again later" });
  }
});

// ─── GET /api/v1/verify/:code ─────────────────────────────────────────────────

router.get("/v1/verify/:code", async (req: Request, res: Response) => {
  const code = String(req.params["code"] ?? "").toUpperCase();
  if (!/^[A-Z0-9]{8}$/.test(code)) {
    return res
      .status(400)
      .json({ found: false, expired: false, error: "Code must be 8 alphanumeric chars" });
  }

  // First check in-memory tx-registry (wallet sends)
  const entry = lookupByCode(code);
  if (entry) {
    const txTime = new Date(entry.timestamp).getTime();
    const expiresAt = txTime + 7 * 86400_000;
    const expired = Date.now() > expiresAt;
    return res.json({
      found: true,
      expired,
      tx: {
        confirmation_code: entry.confirmation_code,
        tx_hash: entry.tx_hash,
        block_height: entry.block_height,
        amount_apr: entry.amount_apr,
        fee_apr: entry.fee_apr,
        from_address: entry.from_address,
        to_address: entry.to_address,
        timestamp: entry.timestamp,
        status: entry.status,
        expires_at: new Date(expiresAt).toISOString(),
      },
    });
  }

  // Fallback: check mint_log by first 8 hex chars of tx_hash (confirmation code)
  try {
    const mintRows = await db
      .select()
      .from(mintLogTable)
      .where(sql`UPPER(LEFT(${mintLogTable.tx_hash}, 8)) = ${code}`)
      .limit(1);

    if (mintRows.length > 0) {
      const row = mintRows[0]!;
      const txTime = row.created_at ? new Date(row.created_at).getTime() : row.ts;
      const expiresAt = txTime + 7 * 86400_000;
      const expired = Date.now() > expiresAt;
      return res.json({
        found: true,
        expired,
        tx: {
          confirmation_code: code,
          tx_hash: row.tx_hash,
          block_height: 0,
          amount_apr: row.amount,
          fee_apr: 0.0001,
          from_address: "",
          to_address: row.to_address,
          timestamp: new Date(txTime).toISOString(),
          status: "confirmed" as const,
          expires_at: new Date(expiresAt).toISOString(),
        },
      });
    }
  } catch (err) {
    logger.warn({ err, code }, "mint_log verify lookup failed");
  }

  return res.json({ found: false, expired: false });
});

// ─── GET /api/v1/transactions/:hash ──────────────────────────────────────────

router.get("/v1/transactions/:hash", async (req: Request, res: Response) => {
  const hash = String(req.params["hash"] ?? "");
  if (!/^[0-9a-f]{64}$/i.test(hash)) {
    return res.status(400).json({ error: "invalid hash: must be 64 hex chars" });
  }
  try {
    const data = await goNodeRest<unknown>(`/api/v1/transactions/${hash.toLowerCase()}`);
    return res.json(data);
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    if (msg.includes("HTTP 404")) return res.status(404).json({ error: "transaction not found" });
    logger.error({ err, hash }, "Failed to fetch transaction from Go node");
    return res.status(502).json({ error: "Blockchain node unavailable — try again later" });
  }
});

// ─── GET /api/v1/address/:address/transactions ────────────────────────────────

router.get("/v1/address/:address/transactions", async (req: Request, res: Response) => {
  const address = String(req.params["address"] ?? "");
  if (!address || address.length < 50) {
    return res.status(400).json({ error: "invalid address" });
  }
  try {
    const data = await goNodeRest<unknown>(`/api/v1/address/${address}/transactions`);
    return res.json(data);
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    if (msg.includes("HTTP 400")) return res.status(400).json({ error: "invalid address" });
    logger.error({ err, address }, "Failed to fetch address txs from Go node");
    return res.status(502).json({ error: "Blockchain node unavailable — try again later" });
  }
});

// ─── GET /api/v1/network/stats ────────────────────────────────────────────────

router.get("/v1/network/stats", async (_req: Request, res: Response) => {
  try {
    const data = await goNodeRest<unknown>("/api/v1/network/stats");
    return res.json(data);
  } catch (err) {
    logger.error({ err }, "Failed to fetch network stats from Go node");
    return res.status(502).json({ error: "Blockchain node unavailable — try again later" });
  }
});

// ─── POST /api/v1/rpc ─────────────────────────────────────────────────────────

interface RpcRequest {
  jsonrpc?: unknown;
  method?: unknown;
  params?: unknown;
  id?: unknown;
}

// Map legacy method names → Go node apr_ methods.
const METHOD_MAP: Record<string, string> = {
  get_block:        "apr_getBlockByHeight",
  get_transaction:  "apr_getTransaction",
  get_balance:      "apr_getBalance",
  send_raw_tx:      "apr_sendRawTransaction",
  get_network_info: "apr_getNodeInfo",
  get_mempool:      "apr_getMempoolTxs",
};

router.post("/v1/rpc", async (req: Request, res: Response) => {
  const body = req.body as RpcRequest;
  const id = body.id ?? null;

  if (body.jsonrpc !== "2.0" || typeof body.method !== "string") {
    return res.status(400).json({
      jsonrpc: "2.0",
      error: {
        code: -32600,
        message: 'Invalid Request — jsonrpc must be "2.0" and method must be a string',
      },
      id,
    });
  }

  const params = (body.params ?? {}) as Record<string, unknown>;

  // If method is already in apr_ namespace, forward directly.
  // Otherwise translate legacy name → apr_ name.
  const goMethod = body.method.startsWith("apr_")
    ? body.method
    : (METHOD_MAP[body.method] ?? null);

  if (!goMethod) {
    return res.json({
      jsonrpc: "2.0",
      error: { code: -32601, message: `Method not found: ${body.method}` },
      id,
    });
  }

  // Special param translation for get_block (legacy uses height/hash fields)
  let goParams: Record<string, unknown> = params;
  if (body.method === "get_block") {
    if (params["height"] !== undefined) {
      goParams = { height: params["height"] };
    } else if (params["hash"] !== undefined) {
      // Use apr_getBlockByHash for hash-based lookups
      try {
        const result = await goNodeRpc("apr_getBlockByHash", { hash: params["hash"] });
        return res.json({ jsonrpc: "2.0", result, id });
      } catch (err: unknown) {
        const msg = err instanceof Error ? err.message : String(err);
        return res.json({
          jsonrpc: "2.0",
          error: { code: -32001, message: msg },
          id,
        });
      }
    } else {
      return res.json({
        jsonrpc: "2.0",
        error: { code: -32602, message: "params must include height (integer) or hash (hex string)" },
        id,
      });
    }
  }

  try {
    const result = await goNodeRpc(goMethod, goParams);
    return res.json({ jsonrpc: "2.0", result, id });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.error({ err, method: goMethod }, "Go node RPC call failed");
    return res.json({
      jsonrpc: "2.0",
      error: { code: -32603, message: msg },
      id,
    });
  }
});

export default router;
