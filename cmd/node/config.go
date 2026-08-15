package main

// config.go — node.yaml write helpers.
//
// persistKeepaliveInterval rewrites the p2p.keepalive_interval field in the
// node.yaml file the node was started with, so an interval tuned live from
// the Admin Panel survives a node restart.  The rewrite is YAML-AST based
// (gopkg.in/yaml.v3 node tree), which keeps comments and key order and stays
// valid for any mapping style (block mappings with any indentation, flow
// mappings, absent sections).  The write is atomic: the new content goes to a
// tmp file in the same directory, is re-parsed via config.Load as a final
// validity check, and only then renamed over the original — a crash or a bad
// rewrite can never leave a broken config behind.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aperod/aperod/config"
)

// setYAMLKeepalive updates (or inserts) p2p.keepalive_interval in the parsed
// YAML document node tree.  Returns an error for document shapes that cannot
// be safely mutated (non-mapping root or non-mapping p2p value).
func setYAMLKeepalive(root *yaml.Node, value string) error {
	// Empty file → build a fresh document with an empty mapping root.
	if root.Kind == 0 || len(root.Content) == 0 {
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return fmt.Errorf("config root is not a YAML mapping (kind=%d)", doc.Kind)
	}

	// Locate the p2p key at top level.
	var p2pVal *yaml.Node
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "p2p" {
			p2pVal = doc.Content[i+1]
			break
		}
	}
	if p2pVal == nil {
		// No p2p section — append one.
		doc.Content = append(doc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "p2p"},
			&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"},
		)
		p2pVal = doc.Content[len(doc.Content)-1]
	}
	// "p2p:" with an empty/null value — turn it into a mapping.
	if p2pVal.Kind == yaml.ScalarNode && (p2pVal.Tag == "!!null" || p2pVal.Value == "") {
		p2pVal.Kind = yaml.MappingNode
		p2pVal.Tag = "!!map"
		p2pVal.Value = ""
		p2pVal.Content = nil
	}
	if p2pVal.Kind != yaml.MappingNode {
		return fmt.Errorf("p2p section is not a YAML mapping (kind=%d)", p2pVal.Kind)
	}

	// Update the existing keepalive_interval scalar, or append one.
	for i := 0; i+1 < len(p2pVal.Content); i += 2 {
		if p2pVal.Content[i].Value == "keepalive_interval" {
			v := p2pVal.Content[i+1]
			v.Kind = yaml.ScalarNode
			v.Tag = "!!str"
			v.Value = value
			v.Content = nil
			return nil
		}
	}
	p2pVal.Content = append(p2pVal.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "keepalive_interval"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
	return nil
}

// persistKeepaliveInterval atomically rewrites (or inserts) the
// p2p.keepalive_interval field in the YAML file at cfgPath.
//
// The document is parsed into a yaml.v3 node tree (comments, key order, and
// mapping styles preserved), mutated, re-encoded, written to a tmp file in
// the same directory, RE-PARSED via config.Load to prove the result is a
// loadable config, and only then renamed over the original.
func persistKeepaliveInterval(cfgPath string, d time.Duration) error {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", cfgPath, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse %s: %w", cfgPath, err)
	}
	if err := setYAMLKeepalive(&root, fmt.Sprintf("%ds", int(d.Seconds()))); err != nil {
		return fmt.Errorf("update %s: %w", cfgPath, err)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return fmt.Errorf("encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode yaml: %w", err)
	}

	// Preserve the original file mode when possible.
	mode := os.FileMode(0644)
	if info, statErr := os.Stat(cfgPath); statErr == nil {
		mode = info.Mode().Perm()
	}

	dir := filepath.Dir(cfgPath)
	tmp, err := os.CreateTemp(dir, ".node.yaml.tmp-*")
	if err != nil {
		return fmt.Errorf("create tmp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { os.Remove(tmpPath) }
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("fsync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		cleanup()
		return fmt.Errorf("chmod tmp: %w", err)
	}

	// Final safety gate: the rewritten file must be loadable as a config and
	// must round-trip the new interval BEFORE it replaces the original.
	newCfg, err := config.Load(tmpPath)
	if err != nil {
		cleanup()
		return fmt.Errorf("rewritten config failed to load — original left untouched: %w", err)
	}
	if newCfg.P2P.KeepaliveInterval != d {
		cleanup()
		return fmt.Errorf(
			"rewritten config round-trip mismatch (got %v, want %v) — original left untouched",
			newCfg.P2P.KeepaliveInterval, d)
	}

	if err := os.Rename(tmpPath, cfgPath); err != nil {
		cleanup()
		return fmt.Errorf("rename tmp over %s: %w", cfgPath, err)
	}
	return nil
}

// readYAMLKeepaliveInterval re-parses the YAML config at cfgPath and returns
// the persisted p2p.keepalive_interval value.  A zero value in the file means
// "use the built-in default (10s)", so 10s is returned in that case — this
// makes the returned value directly comparable to the live interval reported
// by p2p.Host.GetKeepaliveInterval.
func readYAMLKeepaliveInterval(cfgPath string) (time.Duration, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return 0, fmt.Errorf("load %s: %w", cfgPath, err)
	}
	if cfg.P2P.KeepaliveInterval == 0 {
		return 10 * time.Second, nil
	}
	return cfg.P2P.KeepaliveInterval, nil
}
