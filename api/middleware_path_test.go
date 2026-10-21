package api

import "testing"

func TestIsHeavyPathIncludesAddressHistory(t *testing.T) {
	for _, path := range []string{
		"/api/v1/address/aproExample/transactions",
		"/api/v1/address/aproExample/transactions/",
		"/api/v1/address/aproExample/transactions/extra",
		"/api/v1/address/aproExample/utxos",
		"/api/v1/address/aproExample/utxos/",
		"/api/v1/address/aproExample/scan",
		"/api/v1/address/aproExample/scan/",
		"/api/v1/avm/query",
		"/api/v1/avm/estimate-gas",
	} {
		if !isHeavyPath(path) {
			t.Errorf("isHeavyPath(%q) = false, want true", path)
		}
	}
	for _, path := range []string{
		"/api/v1/address/aproExample",
		"/api/v1/status",
	} {
		if isHeavyPath(path) {
			t.Errorf("isHeavyPath(%q) = true, want false", path)
		}
	}
}
