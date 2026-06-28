import React, { useState } from "react";
import { Copy, Check, Terminal, Server, Zap, Globe, Key, BookOpen, Code2 } from "lucide-react";
import { useI18n } from "@/lib/i18n";

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      onClick={() => {
        navigator.clipboard.writeText(text);
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      }}
      className="absolute top-3 right-3 p-1.5 rounded-md bg-white/10 hover:bg-white/20 transition-colors text-slate-300 hover:text-white"
    >
      {copied ? <Check className="h-3.5 w-3.5 text-green-400" /> : <Copy className="h-3.5 w-3.5" />}
    </button>
  );
}

function CodeBlock({ code, lang = "bash" }: { code: string; lang?: string }) {
  return (
    <div className="relative rounded-xl bg-slate-900 overflow-hidden text-sm">
      <div className="flex items-center gap-2 px-4 py-2.5 bg-slate-800 border-b border-slate-700">
        <Terminal className="h-3.5 w-3.5 text-slate-400" />
        <span className="text-xs text-slate-400 font-mono">{lang}</span>
      </div>
      <div className="relative">
        <pre className="p-4 overflow-x-auto text-slate-100 font-mono text-xs leading-relaxed whitespace-pre scrollbar-hide">
          <code>{code}</code>
        </pre>
        <CopyButton text={code} />
      </div>
    </div>
  );
}

function SectionTitle({ icon, title, sub }: { icon: React.ReactNode; title: string; sub?: string }) {
  return (
    <div className="flex items-start gap-3 mb-6">
      <div className="p-2.5 rounded-xl bg-primary/10 text-primary shrink-0 mt-0.5">{icon}</div>
      <div>
        <h2 className="text-xl font-bold text-slate-900">{title}</h2>
        {sub && <p className="text-muted-foreground text-sm mt-0.5">{sub}</p>}
      </div>
    </div>
  );
}

function Step({ n, title, children }: { n: number; title: string; children: React.ReactNode }) {
  return (
    <div className="flex gap-4">
      <div className="shrink-0 w-8 h-8 rounded-full bg-primary text-white flex items-center justify-center text-sm font-bold">
        {n}
      </div>
      <div className="pb-6 border-b border-border last:border-0 last:pb-0 flex-1">
        <h3 className="font-semibold text-slate-900 mb-2">{title}</h3>
        {children}
      </div>
    </div>
  );
}

const RAW = `https://raw.githubusercontent.com/aperod-network/aperod-node/main`;

const NODE_INSTALL_AUTO = `# Ubuntu 22.04 / 24.04 / Debian 12 — full node:
curl -fsSL ${RAW}/deploy/install-node.sh | sudo bash`;

const NODE_INSTALL_VALIDATOR = `curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-validator.sh | sudo bash`;

const NODE_INSTALL_VALIDATOR_CI = `APEROD_REWARD_ADDRESS=<your-apr-address> bash <(curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-validator.sh)`;

const NODE_INSTALL = `# Manual build (requires Go 1.22+):
git clone https://github.com/aperod-network/aperod-node.git
cd aperod-node

make deps
make build

./build/aperod-node --help
./build/aperod --help`;

const NODE_CONFIG = `# /etc/aperod/node.yaml  (auto-created by install script)
network: testnet
data_dir: /var/lib/aperod
log_level: info

p2p:
  listen_addr: /ip4/0.0.0.0/tcp/30303
  bootnodes:
    - /ip4/77.221.153.86/tcp/30303
  max_peers: 50

consensus:
  validator_key: /etc/aperod/validator.key
  reward_address: <your-apr-address-from-telegram-wallet>

api:
  enabled: true
  listen_addr: 127.0.0.1:8545     # RPC port (localhost only)

genesis:
  file: /etc/aperod/genesis-testnet.yaml`;

const VALIDATOR_KEYS = `# ── BEFORE INSTALLING THE NODE ──────────────────────────────────
# Get your APR wallet address from the Telegram bot:
#   https://t.me/aperod_bot  →  Create wallet  →  Copy APR address
# Block rewards will go directly to this address.

# ── INSTALL ──────────────────────────────────────────────────────
# The installer asks for your APR address, generates a consensus key
# (for block signing only — no spending ability), and starts the node.
# At the end it prints the ready-to-run registration command:

curl -s -X POST https://aperod.com/api/validators/apply \\
  -H 'Content-Type: application/json' \\
  -d '{
    "pubKey":   "<consensus-pubkey-from-installer>",
    "alias":    "my-validator",
    "endpoint": "/ip4/<your-ip>/tcp/30303",
    "address":  "<your-apr-address-from-telegram>"
  }'

# ── STAKE ────────────────────────────────────────────────────────
# Transfer at least 100,000 APR to your wallet address.
# Node activates automatically at the next epoch (~100 blocks, ≈1.7 min).
# You receive a Telegram notification for every block reward.`;

const DOCKER_COMPOSE = `version: "3.8"
services:
  node1:
    image: ghcr.io/aperod-network/aperod-node:latest
    ports: ["30303:30303"]
    volumes: ["./data/node1:/var/lib/aperod"]
    environment:
      - NODE_ID=1

  node2:
    image: ghcr.io/aperod-network/aperod-node:latest
    ports: ["30304:30303"]
    volumes: ["./data/node2:/var/lib/aperod"]
    environment:
      - NODE_ID=2
      - BOOTSTRAP_PEER=node1:30303

  node3:
    image: ghcr.io/aperod-network/aperod-node:latest
    ports: ["30305:30303"]
    volumes: ["./data/node3:/var/lib/aperod"]
    environment:
      - NODE_ID=3
      - BOOTSTRAP_PEER=node1:30303

  node4:
    image: ghcr.io/aperod-network/aperod-node:latest
    ports: ["30306:30303"]
    volumes: ["./data/node4:/var/lib/aperod"]
    environment:
      - NODE_ID=4
      - BOOTSTRAP_PEER=node1:30303`;

const UNINSTALL_CMD = `# Interactive (asks for confirmation — accepts yes / YES / Yes):
bash <(curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/uninstall-validator.sh)

# Non-interactive (automated / CI):
APEROD_UNINSTALL_CONFIRM=YES bash <(curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/uninstall-validator.sh)`;

const API_AUTH = `# All write endpoints require X-API-Key header
curl -X POST http://<node-ip>:8545/api/v1/wallet/send \\
  -H "X-API-Key: your-api-key-here" \\
  -H "Content-Type: application/json" \\
  -d '{"to": "APR1...", "amount": 100}'`;

const API_STATS = `# GET /api/v1/stats
curl http://<node-ip>:8545/api/v1/stats

{
  "height": 12345,
  "tps_last_10": 3.67,
  "mempool_count": 4,
  "block_time_secs": 3,
  "tip_time": "2026-01-01T12:00:00Z"
}`;

const API_TX = `# GET /api/v1/transactions/:hash
curl http://<node-ip>:8545/api/v1/transactions/ab3f...9d72

{
  "hash": "ab3f...9d72",
  "block_height": 12344,
  "status": "confirmed",
  "fee": 1500,
  "inputs": 11,
  "outputs": 2,
  "pending": false
}`;

const API_SEND = `# POST /api/v1/wallet/send
curl -X POST http://<node-ip>:8545/api/v1/wallet/send \\
  -H "X-API-Key: your-api-key" \\
  -H "Content-Type: application/json" \\
  -d '{
    "from_address": "APR1...",
    "to_address": "APR1...",
    "amount": 5000000000,
    "payment_id": "optional-tracking-id"
  }'

# Response
{
  "tx_hash": "deadbeef...",
  "fee": 1200,
  "status": "broadcast"
}`;

const SSE_EVENTS = `// Subscribe to live block events (Server-Sent Events)
const es = new EventSource('http://<node-ip>:8545/api/v1/events');

es.onmessage = (event) => {
  const { topic, data } = JSON.parse(event.data);

  if (topic === 'new_block') {
    console.log('New block:', data.height, data.hash);
    // Update your exchange order book, check balances, etc.
  }
};

// Topics: connected | new_block | heartbeat`;

const RPC_EXAMPLE = `POST /api/v1/rpc
Content-Type: application/json

{
  "jsonrpc": "2.0",
  "method": "get_block",
  "params": { "height": 12345 },
  "id": 1
}

// Response
{
  "jsonrpc": "2.0",
  "result": {
    "hash": "ab3f...9d72",
    "height": 12345,
    "tx_count": 4,
    "validator_pub": "a3f9..."
  },
  "id": 1
}`;

export default function Docs() {
  const { t } = useI18n();
  const [activeTab, setActiveTab] = useState<"node" | "api" | "rpc">("node");

  const tabs = [
    { id: "node" as const, label: t.docs.tabNode, icon: <Server className="h-4 w-4" /> },
    { id: "api" as const, label: t.docs.tabApi, icon: <Globe className="h-4 w-4" /> },
    { id: "rpc" as const, label: t.docs.tabRpc, icon: <Code2 className="h-4 w-4" /> },
  ];

  return (
    <div className="max-w-5xl mx-auto px-4 sm:px-6 py-8 animate-in fade-in duration-300">
      {/* Header */}
      <div className="mb-8">
        <div className="flex items-center gap-3 mb-2">
          <div className="p-2 rounded-xl bg-primary/10 text-primary">
            <BookOpen className="h-6 w-6" />
          </div>
          <h1 className="text-3xl font-bold text-slate-900">{t.docs.title}</h1>
        </div>
        <p className="text-muted-foreground text-base ml-14">{t.docs.subtitle}</p>
      </div>

      {/* Tab navigation */}
      <div className="flex gap-1 p-1 bg-muted rounded-xl mb-8 w-fit">
        {tabs.map(tab => (
          <button
            key={tab.id}
            onClick={() => setActiveTab(tab.id)}
            className={`flex items-center gap-2 px-4 py-2 rounded-lg text-sm font-medium transition-all ${
              activeTab === tab.id
                ? "bg-background text-primary shadow-sm"
                : "text-muted-foreground hover:text-foreground"
            }`}
          >
            {tab.icon}
            {tab.label}
          </button>
        ))}
      </div>

      {/* Node Setup Tab */}
      {activeTab === "node" && (
        <div className="space-y-10">
          <SectionTitle icon={<Server className="h-5 w-5" />} title={t.docs.nodeTitle} sub={t.docs.nodeSub} />

          {/* Requirements */}
          <div className="section-card p-6">
            <h3 className="font-semibold text-slate-900 mb-4 flex items-center gap-2">
              <span className="w-5 h-5 rounded-full bg-emerald-100 text-emerald-700 flex items-center justify-center text-xs font-bold">✓</span>
              {t.docs.req}
            </h3>
            <ul className="space-y-2">
              {[t.docs.reqItem1, t.docs.reqItem2, t.docs.reqItem3, t.docs.reqItem4].map((item, i) => (
                <li key={i} className="flex items-start gap-2 text-sm text-foreground">
                  <span className="text-primary font-bold mt-0.5">→</span>
                  <span>{item}</span>
                </li>
              ))}
            </ul>
          </div>

          {/* Steps */}
          <div className="space-y-6">
            <Step n={1} title={t.docs.installTitle}>
              {/* ── Validator node — primary / most users ── */}
              <p className="text-sm font-semibold text-foreground mb-1">{t.docs.installValidatorLabel}</p>
              <div className="mb-3 rounded-lg bg-blue-50 border border-blue-200 px-4 py-2.5 text-sm text-blue-800">
                ℹ️ {t.docs.installValidatorHint}
              </div>
              <CodeBlock code={NODE_INSTALL_VALIDATOR} />
              <details className="mt-3">
                <summary className="text-sm text-muted-foreground cursor-pointer hover:text-foreground">{t.docs.installValidatorCiTitle}</summary>
                <div className="mt-2">
                  <CodeBlock code={NODE_INSTALL_VALIDATOR_CI} />
                </div>
              </details>

              {/* ── Full node — secondary / archive ── */}
              <details className="mt-5">
                <summary className="text-sm font-medium text-muted-foreground cursor-pointer hover:text-foreground">{t.docs.installFullNodeLabel}</summary>
                <div className="mt-2">
                  <CodeBlock code={NODE_INSTALL_AUTO} />
                </div>
              </details>

              <div className="mt-4 flex items-center gap-3">
                <a
                  href="https://github.com/aperod-network/aperod-node"
                  target="_blank"
                  rel="noopener noreferrer"
                  className="inline-flex items-center gap-2 px-4 py-2 rounded-lg bg-slate-900 text-white text-sm font-medium hover:bg-slate-700 transition-colors"
                >
                  <svg className="h-4 w-4" viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.37 0 0 5.37 0 12c0 5.31 3.435 9.795 8.205 11.385.6.105.825-.255.825-.57 0-.285-.015-1.23-.015-2.235-3.015.555-3.795-.735-4.035-1.41-.135-.345-.72-1.41-1.23-1.695-.42-.225-1.02-.78-.015-.795.945-.015 1.62.87 1.845 1.23 1.08 1.815 2.805 1.305 3.495.99.105-.78.42-1.305.765-1.605-2.67-.3-5.46-1.335-5.46-5.925 0-1.305.465-2.385 1.23-3.225-.12-.3-.54-1.53.12-3.18 0 0 1.005-.315 3.3 1.23.96-.27 1.98-.405 3-.405s2.04.135 3 .405c2.295-1.56 3.3-1.23 3.3-1.23.66 1.65.24 2.88.12 3.18.765.84 1.23 1.905 1.23 3.225 0 4.605-2.805 5.625-5.475 5.925.435.375.81 1.095.81 2.22 0 1.605-.015 2.895-.015 3.3 0 .315.225.69.825.57A12.02 12.02 0 0 0 24 12c0-6.63-5.37-12-12-12z"/></svg>
                  github.com/aperod-network/aperod-node
                </a>
                <span className="text-xs text-muted-foreground">{t.docs.installManualHint}</span>
              </div>
              <details className="mt-4">
                <summary className="text-sm text-muted-foreground cursor-pointer hover:text-foreground">{t.docs.installManualTitle}</summary>
                <div className="mt-3">
                  <CodeBlock code={NODE_INSTALL} />
                </div>
              </details>
            </Step>

            <Step n={2} title={t.docs.configTitle}>
              <p className="text-sm text-muted-foreground mb-3">{t.docs.configDesc}</p>
              <CodeBlock code={NODE_CONFIG} lang="yaml" />
            </Step>

            <Step n={3} title={t.docs.validatorTitle}>
              <p className="text-sm text-muted-foreground mb-3">{t.docs.validatorDesc}</p>
              <ul className="space-y-1 mb-4">
                {[t.docs.validatorStep1, t.docs.validatorStep2, t.docs.validatorStep3].map((s, i) => (
                  <li key={i} className="text-sm text-slate-600 flex items-center gap-2">
                    <span className="w-5 h-5 rounded-full bg-primary/10 text-primary flex items-center justify-center text-xs font-bold shrink-0">{i + 1}</span>
                    {s}
                  </li>
                ))}
              </ul>
              <CodeBlock code={VALIDATOR_KEYS} />
            </Step>

            <Step n={4} title={t.docs.dockerTitle}>
              <p className="text-sm text-muted-foreground mb-3">{t.docs.dockerDesc}</p>
              <CodeBlock code={DOCKER_COMPOSE} lang="yaml" />
            </Step>

            <Step n={5} title={t.docs.uninstallTitle}>
              <p className="text-sm text-muted-foreground mb-3">{t.docs.uninstallDesc}</p>
              <CodeBlock code={UNINSTALL_CMD} />
              <div className="mt-3 rounded-lg bg-amber-50 border border-amber-200 px-4 py-3 text-sm text-amber-800">
                ⚠️ {t.docs.uninstallWarning}
              </div>
            </Step>
          </div>
        </div>
      )}

      {/* Exchange API Tab */}
      {activeTab === "api" && (
        <div className="space-y-10">
          <SectionTitle icon={<Globe className="h-5 w-5" />} title={t.docs.apiTitle} sub={t.docs.apiSub} />

          {/* Authentication */}
          <div>
            <h3 className="font-semibold text-slate-900 mb-2 flex items-center gap-2">
              <Key className="h-4 w-4 text-amber-500" />
              {t.docs.authTitle}
            </h3>
            <p className="text-sm text-muted-foreground mb-3">{t.docs.authDesc}</p>
            <CodeBlock code={API_AUTH} />
          </div>

          {/* Endpoints list */}
          <div className="section-card p-6">
            <h3 className="font-semibold text-slate-900 mb-4">{t.docs.endpointsTitle}</h3>
            <ul className="space-y-3">
              {[t.docs.ep1, t.docs.ep2, t.docs.ep3, t.docs.ep4, t.docs.ep5, t.docs.ep6].map((ep, i) => {
                const [method, ...rest] = ep.split(" ");
                return (
                  <li key={i} className="flex items-start gap-2 text-sm">
                    <span className={`shrink-0 px-1.5 py-0.5 rounded text-xs font-bold font-mono ${
                      method === "GET" ? "bg-emerald-100 text-emerald-700" : "bg-blue-100 text-blue-700"
                    }`}>
                      {method}
                    </span>
                    <span className="text-foreground">{rest.join(" ")}</span>
                  </li>
                );
              })}
            </ul>
          </div>

          {/* Code examples */}
          <div className="space-y-6">
            <div>
              <h3 className="font-semibold text-slate-900 mb-3 flex items-center gap-2">
                <Zap className="h-4 w-4 text-primary" />
                GET /api/v1/stats
              </h3>
              <CodeBlock code={API_STATS} lang="json" />
            </div>
            <div>
              <h3 className="font-semibold text-slate-900 mb-3">GET /api/v1/transactions/:hash</h3>
              <CodeBlock code={API_TX} lang="json" />
            </div>
            <div>
              <h3 className="font-semibold text-slate-900 mb-3">POST /api/v1/wallet/send</h3>
              <CodeBlock code={API_SEND} lang="json" />
            </div>
          </div>

          {/* Rate limits */}
          <div className="rounded-xl border border-amber-200 bg-amber-50 p-4 text-sm text-amber-800">
            <p className="font-semibold mb-1">{t.docs.rateTitle}</p>
            <p>{t.docs.rateDesc}</p>
          </div>

          {/* SSE */}
          <div>
            <h3 className="font-semibold text-slate-900 mb-2">{t.docs.webhookTitle}</h3>
            <p className="text-sm text-muted-foreground mb-3">{t.docs.webhookDesc}</p>
            <CodeBlock code={SSE_EVENTS} lang="javascript" />
          </div>
        </div>
      )}

      {/* JSON-RPC Tab */}
      {activeTab === "rpc" && (
        <div className="space-y-8">
          <SectionTitle icon={<Code2 className="h-5 w-5" />} title={t.docs.rpcTitle} sub={t.docs.rpcSub} />

          <div className="rounded-xl border border-blue-200 bg-blue-50 p-4 text-sm text-blue-800">
            <p className="font-semibold mb-1">ℹ️ {t.docs.rpcNote}</p>
          </div>

          <CodeBlock code={RPC_EXAMPLE} lang="json" />

          {/* Method table */}
          <div className="section-card overflow-hidden">
            <div className="section-header">
              <span className="text-sm font-semibold text-foreground">JSON-RPC Methods</span>
            </div>
            <div className="overflow-x-auto scrollbar-hide">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-border bg-slate-50">
                    <th className="text-left px-4 py-3 text-xs font-semibold text-muted-foreground uppercase tracking-wider">Method</th>
                    <th className="text-left px-4 py-3 text-xs font-semibold text-muted-foreground uppercase tracking-wider">Params</th>
                    <th className="text-left px-4 py-3 text-xs font-semibold text-muted-foreground uppercase tracking-wider">Description</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {[
                    { method: "get_block", params: "height | hash", desc: "Get block by height or hash" },
                    { method: "get_transaction", params: "hash", desc: "Get transaction details" },
                    { method: "get_balance", params: "address", desc: "Get address balance" },
                    { method: "send_raw_tx", params: "tx_hex", desc: "Broadcast raw transaction" },
                    { method: "get_network_info", params: "—", desc: "Network statistics" },
                    { method: "get_mempool", params: "limit?", desc: "List pending transactions" },
                    { method: "get_block_template", params: "validator_pub", desc: "Get block template for mining" },
                  ].map(row => (
                    <tr key={row.method} className="hover:bg-slate-50 transition-colors">
                      <td className="px-4 py-3">
                        <code className="text-xs font-mono bg-muted text-primary px-1.5 py-0.5 rounded">{row.method}</code>
                      </td>
                      <td className="px-4 py-3 text-xs font-mono text-muted-foreground">{row.params}</td>
                      <td className="px-4 py-3 text-xs text-slate-600">{row.desc}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
