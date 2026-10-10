package main

// config_reload.go — live config reload triggered by SIGHUP.
//
// Only snapshot.scan_checkpoint_interval is updated on SIGHUP because that is
// the value an operator needs to tune without restarting (memory vs. crash-
// recovery speed).  Settings that require structural changes (listen addresses,
// key paths, network, data_dir) are intentionally excluded — changing those
// live would corrupt running subsystems.

import (
        "log/slog"

        "github.com/aperod/aperod/config"
)

// reloadScanCheckpointInterval re-reads the YAML config at cfgPath and, if
// the parse succeeds, updates cfg.Snapshot.ScanCheckpointInterval in-place.
//
// The updated value is used the next time runStartupScan is called (i.e. after
// the node restarts following a crash mid-scan).  No restart is required for
// the setting to be persisted in memory — a subsequent SIGTERM/SIGINT will
// write a shutdown snapshot; the next boot's startup scan picks up the new
// interval from the already-updated cfg.
//
// On any parse or validation error the existing value is left unchanged and a
// warning is logged so the operator knows the reload was a no-op.
func reloadScanCheckpointInterval(cfgPath string, cfg *config.Config, log *slog.Logger) {
        newCfg, err := config.Load(cfgPath)
        if err != nil {
                log.Warn("SIGHUP: config reload failed — keeping current ScanCheckpointInterval",
                        "err", err,
                        "current_interval", cfg.Snapshot.ScanCheckpointInterval,
                )
                return
        }
        old := cfg.Snapshot.ScanCheckpointInterval
        cfg.Snapshot.ScanCheckpointInterval = newCfg.Snapshot.ScanCheckpointInterval
        log.Info("SIGHUP: ScanCheckpointInterval reloaded",
                "old", old,
                "new", cfg.Snapshot.ScanCheckpointInterval,
        )
}
