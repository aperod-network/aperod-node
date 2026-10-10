# Aperod Node Maintenance Guide

Operational playbook for keeping all **21 validators** and relay nodes healthy —
disk space, log rotation, chain resync, automatic recovery.

---

## Table of Contents

- [Automated safeguards (always-on)](#automated-safeguards-always-on)
- [Disk space management](#disk-space-management)
- [Log rotation setup (one-time per server)](#log-rotation-setup-one-time-per-server)
- [Pruning old blocks (relay nodes)](#pruning-old-blocks-relay-nodes)
- [Chain resync — relay node fell far behind](#chain-resync--relay-node-fell-far-behind)
- [Emergency disk-full recovery](#emergency-disk-full-recovery)
- [Validator checklist after reinstall](#validator-checklist-after-reinstall)

---

## Automated safeguards (always-on)

The following services run automatically on every validator after installation
via `install-validator.sh` or `install-node.sh`:

| Service | What it does | Fires every |
|---------|-------------|-------------|
| `aperod-node-watchdog.timer` | Restarts node if API fails, RAM > threshold, blocks stall, or disk < threshold | 60 s |
| `aperod-node-sched-restart.timer` | Nightly proactive restart to prevent RAM growth | 04:00 UTC |
| `aperod-backup.timer` | Snapshot backup to backup directory | configurable |

The watchdog now also monitors **disk space** and sends a Telegram alert when
free space drops below 15 % (`DISK_WARN_PCT`) and auto-cleans log files when
it drops below 5 % (`DISK_CRIT_PCT`).

To verify the watchdog is installed and running:

```bash
systemctl status aperod-node-watchdog.timer
# → should show: active (waiting)
```

---

## Disk space management

### Check current disk usage

```bash
df -h /
du -sh /var/log/syslog /var/log/daemon.log 2>/dev/null
du -sh /var/lib/aperod/chain.db 2>/dev/null || \
  du -sh /opt/aperod/data/testnet/chain.db 2>/dev/null
journalctl --disk-usage
```

### Emergency: disk is full right now

```bash
# 1. Truncate the largest log offenders (rsyslog keeps fd open — truncate, not rm)
truncate -s 0 /var/log/syslog /var/log/daemon.log \
              /var/log/syslog.1 /var/log/daemon.log.1 2>/dev/null || true

# 2. Trim journald to 500 MB
journalctl --vacuum-size=500M

# 3. Clear apt cache
apt-get clean 2>/dev/null || true

# Confirm space freed:
df -h /
```

### Adjust watchdog disk thresholds

Edit `/etc/aperod/watchdog.env` (or the watchdog systemd drop-in):

```bash
DISK_WARN_PCT=15     # Telegram alert when free space < 15 %
DISK_CRIT_PCT=5      # Auto-cleanup syslog when free space < 5 %
DISK_CHECK_PATH=/    # Filesystem to monitor (default: /)
```

---

## Log rotation setup (one-time per server)

Install on **every validator and relay node** to prevent syslog from growing
unboundedly during crash-restart loops:

```bash
# 1. Copy the logrotate config
sudo cp /opt/aperod/blockchain/deploy/aperod-syslog.logrotate \
        /etc/logrotate.d/aperod-syslog

# 2. Dry-run to verify syntax
sudo logrotate -d /etc/logrotate.d/aperod-syslog

# 3. Limit journald storage
echo 'SystemMaxUse=500M' | sudo tee -a /etc/systemd/journald.conf
sudo systemctl restart systemd-journald
```

Force immediate rotation if log files are already large:

```bash
sudo logrotate -f /etc/logrotate.d/aperod-syslog
```

---

## Pruning old blocks (relay nodes)

Relay nodes do **not** need to store full block history. Enabling pruning caps
`chain.db` at ~300 MB instead of growing unboundedly.

**One-command setup** (run on the relay node):

```bash
sudo bash /opt/aperod/blockchain/deploy/enable-pruning.sh --keep-blocks=100000
```

This adds the following to `/etc/aperod/node.yaml` and restarts the node:

```yaml
pruning:
  mode: light
  keep_blocks: 100000   # ~3.5 days at 3 s/block
```

To verify pruning is active after restart:

```bash
journalctl -u aperod-node -n 20 --no-pager | grep -i prun
```

---

## Chain resync — relay node fell far behind

When a relay node's chain tip is more than ~50 000 blocks behind the validator
(e.g. after a disk-full crash), P2P sync cannot bridge the gap reliably. The
fastest recovery is to rsync the chain data directly from the validator.

### Automated resync script

Run **on the SOURCE validator** (the one with the authoritative chain data):

```bash
sudo bash /opt/aperod/blockchain/deploy/node-resync.sh \
    --source /opt/aperod/data/testnet \
    --target root@<relay-ip>:/var/lib/aperod
```

What happens:
1. Stops `aperod-node` on the validator (~3–5 min downtime for block production)
2. rsyncs `chain.db` + `snapshot-v2-*.json.gz` to the relay
3. Removes stale `p2p_whitelist.json` / `p2p_bans.json` on the relay
4. Restarts both nodes
5. Sends a Telegram success/failure alert

Estimated downtime: **3–5 minutes** (depends on chain.db size and network speed).

### Manual step-by-step (if the script is unavailable)

```bash
# ── On the VALIDATOR (source) ─────────────────────────────────────────
systemctl stop aperod-node

rsync -az --progress \
  /opt/aperod/data/testnet/chain.db \
  /opt/aperod/data/testnet/snapshot-v2-*.json.gz \
  root@<relay-ip>:/var/lib/aperod/

systemctl start aperod-node

# ── On the RELAY (target) ─────────────────────────────────────────────
rm -f /var/lib/aperod/p2p_whitelist.json \
      /var/lib/aperod/p2p_bans.json
systemctl restart aperod-node

# Verify sync started:
journalctl -u aperod-node -n 20 --no-pager | grep -i "tip\|sync\|peer"
```

> **Important**: always stop the source before rsync.
> Copying a live LevelDB produces a corrupt database.

---

## Emergency disk-full recovery

If `aperod-node` is in a crash-restart loop with `no space left on device`:

```bash
# Step 1 — stop the crash-loop to prevent further log accumulation
systemctl stop aperod-node

# Step 2 — free disk space (usually syslog from crash-loop logging)
truncate -s 0 /var/log/syslog /var/log/daemon.log \
              /var/log/syslog.1 /var/log/daemon.log.1 2>/dev/null || true
journalctl --vacuum-size=500M
apt-get clean 2>/dev/null || true

# Step 3 — confirm space freed
df -h /
# → Must be at least 2 GB free for LevelDB to operate

# Step 4 — restart the node
systemctl start aperod-node
sleep 5
systemctl status aperod-node --no-pager | head -5

# Step 5 — install logrotate to prevent recurrence (see above)
sudo cp /opt/aperod/blockchain/deploy/aperod-syslog.logrotate \
        /etc/logrotate.d/aperod-syslog
```

---

## Validator checklist after reinstall

After provisioning a new server or recovering from a major failure, verify:

```bash
# 1. Node data directory has correct permissions
ls -la /var/lib/aperod/ || ls -la /opt/aperod/data/testnet/

# 2. Validator key is present and has correct permissions
ls -la /opt/aperod/blockchain/data/testnet/validator.key
# → Must be: -rw------- (chmod 600)
# Fix with: chmod 600 /opt/aperod/blockchain/data/testnet/validator.key

# 3. Node is running and producing blocks
curl -s http://localhost:8545/api/v1/status | python3 -m json.tool | \
  grep -E "height|syncing|ok"

# 4. Watchdog is active
systemctl status aperod-node-watchdog.timer --no-pager | grep Active

# 5. Log rotation is configured
ls /etc/logrotate.d/aperod-syslog && echo "OK" || echo "MISSING — install it"

# 6. Pruning configured (relay nodes only)
grep -A3 "^pruning:" /etc/aperod/node.yaml || echo "pruning not configured"

# 7. journald disk limit is set
grep SystemMaxUse /etc/systemd/journald.conf || echo "WARNING: no journald size limit"

# 8. Peer whitelist includes the validator IP
grep -A5 "peer_whitelist:" /etc/aperod/node.yaml
```

---

*Maintained alongside `aperod-node` source — [aperod-network](https://github.com/aperod-network)*
