package deploy_test

import "testing"

func TestUpgradeNodeE2E(t *testing.T) {
	runNodeLifecycle(t, "upgrade")
}
