package deploy_test

import "testing"

func TestUpdateNodeE2E(t *testing.T) {
	runNodeLifecycle(t, "update")
}
