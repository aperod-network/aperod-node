# Aperod Node Maintenance Guide

Mainnet operations guide for existing validator and relay nodes. For
bootstrap and chain identity requirements, see [Mainnet](MAINNET.md).

---

## Table of Contents

- [Automated safeguards](#automated-safeguards)
- [Disk space management](#disk-space-management)
- [Log rotation setup (one-time per server)](#log-rotation-setup-one-time-per-server)
- [Pruning old blocks (relay nodes)](#pruning-old-blocks-relay-nodes)
- [Chain resync — relay node fell far behind](#chain-resync--relay-node-fell-far-behind)
- [Emergency disk-full recovery](#emergency-disk-full-recovery)
- [Validator checklist after reinstall](#validator-checklist-after-reinstall)

---

## Automated safeguards

Check which services your deployment actually installed before relying on
automated recovery or backups:

| Service | What it does | Fires every |
|---------|-------------|-------------|
| `aperod-node-watchdog.timer` | Restarts node if API fails, RAM > threshold, blocks stall, or disk < threshold | 60 s |
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
# Read the effective data-directory setting of the running service first.
sudo systemctl cat aperod-node
# Set MAINNET_DATA_DIR to the path confirmed in that service's configuration.
du -sh -- "${MAINNET_DATA_DIR:?Set the verified Mainnet data directory}/chain.db"
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

First compare the relay's last known canonical block hash with the
trusted Mainnet source, then check peer admission and the source's block
coverage. Distance from the tip alone does not establish a fork or show
that P2P recovery is impossible.

If the needed history is unavailable from peers, use a verified, closed
Mainnet checkpoint following [Mainnet](MAINNET.md). Import it into a
separate staged directory and verify the signed genesis and checkpoint
hashes before starting the relay. Keep its previous data until the new
copy advances with matching canonical hashes and passes a database
reopen check. Do not copy a live LevelDB, stop the only producer merely
to resync a relay, or delete peer allowlists as part of a data restore.

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

These checks are for an already authorized validator, not a registration
procedure. Use the configured paths and API port from that node's service.

```bash
# 1. Node data directory has correct permissions
stat -c '%A %U %n' -- "${MAINNET_DATA_DIR:?Set the verified Mainnet data directory}"

# 2. Validator key is present and has correct permissions
stat -c '%a %U %n' -- "${VALIDATOR_KEY_PATH:?Set the configured validator key path}"
# Expect mode 600 and ownership by the node service account.

# 3. Node is responding; also verify its canonical hash and active set.
curl -fsS "${NODE_API_URL:?Set the configured loopback API URL}/api/v1/status" | \
  python3 -m json.tool

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
