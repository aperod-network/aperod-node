// SPDX-License-Identifier: Apache-2.0
package deploy_test

import (
	"os/exec"
	"testing"
)

func TestPrivilegedToolTrustBoundary(t *testing.T) {
	cmd := exec.Command("python3", "test-privileged-tool-trust.py")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("privileged tool trust regressions: %v\n%s", err, output)
	}
}
