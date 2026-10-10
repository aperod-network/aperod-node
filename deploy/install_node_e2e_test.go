package deploy_test

import "testing"

func TestInstallNodeE2E(t *testing.T) {
	runNodeLifecycle(t, "install")
}
