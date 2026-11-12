package main

import (
	"fmt"
	"strings"
)

// explicitMaintenanceDataDir is shared by the early, config-free mutation
// commands. Never infer their target from ./data, environment or node.yaml:
// selecting a chain for compaction/repair must be an explicit operator action.
func explicitMaintenanceDataDir(args []string, command string) (string, error) {
	var dataDir string
	found := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg != "--data-dir" && !strings.HasPrefix(arg, "--data-dir=") {
			continue
		}
		if found {
			return "", fmt.Errorf("%s: --data-dir must be supplied exactly once; refusing an ambiguous maintenance target", command)
		}
		found = true
		if arg == "--data-dir" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return "", fmt.Errorf("%s: --data-dir requires a path, not another flag", command)
			}
			i++
			dataDir = args[i]
		} else {
			dataDir = strings.TrimPrefix(arg, "--data-dir=")
		}
		if strings.TrimSpace(dataDir) == "" {
			return "", fmt.Errorf("%s: --data-dir must not be empty", command)
		}
	}
	if !found {
		return "", fmt.Errorf("%s requires --data-dir=<path>; refusing to infer a maintenance target from ./data or --config", command)
	}
	return dataDir, nil
}
