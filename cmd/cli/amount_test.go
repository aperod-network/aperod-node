package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/internal/testutil"
)

func TestParseAPROAmount_ExactQuantumAndWireBoundary(t *testing.T) {
	for _, tc := range []struct {
		text string
		want uint64
	}{
		{"0.00000001", 1}, {"0.00000029", 29}, {"1.00000001", 100_000_001},
		{"100000", 10_000_000_000_000}, {"1e5", 10_000_000_000_000},
		{"+.29e-6", 29}, {"1.0000000000", 100_000_000},
		{"184467440737.09551615", math.MaxUint64},
	} {
		got, err := parseAPROAmount(tc.text)
		if err != nil || got != tc.want {
			t.Errorf("parse(%q)=%d, %v; want %d", tc.text, got, err, tc.want)
		}
	}
	legacyFloat, err := strconv.ParseFloat("0.00000029", 64)
	testutil.NoError(t, err)
	if uint64(legacyFloat*1e8) == 29 {
		t.Fatal("the negative control no longer reproduces the legacy float truncation")
	}
}

func TestParseAPROAmount_RejectsLossOverflowAndUnboundedExponent(t *testing.T) {
	for _, text := range []string{
		"", "0", "-1", "NaN", "Inf", "+Inf", "1/2", "0x1p2",
		"0.000000001", "1.000000001", "184467440737.09551616",
		"1e999999999", "1e-999999999", "1e257", "1e-257", strings.Repeat("1", 257),
	} {
		if value, err := parseAPROAmount(text); err == nil || value != 0 {
			t.Errorf("accepted invalid amount %q: value=%d err=%v", text, value, err)
		}
	}
}

func FuzzParseAPROAmountRoundTrip(f *testing.F) {
	for _, seed := range []string{"0.00000029", "1e5", "184467440737.09551615", "NaN", "1e999999999"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		value, err := parseAPROAmount(text)
		if err != nil {
			return
		}
		again, err := parseAPROAmount(formatAPROAmount(value))
		if err != nil || value == 0 || again != value {
			t.Fatalf("exact amount did not round-trip: value=%d again=%d err=%v", value, again, err)
		}
	})
}

func TestCLISend_UnimplementedPathNeverClaimsSubmission(t *testing.T) {
	var point crypto.Point32
	for i := range point {
		point[i] = 0x66
	}
	point[0] = 0x58 // Canonical compressed Ed25519 base point, not a private key.
	address := crypto.EncodeAddress(crypto.TestnetByte, point, point)
	if err := crypto.Validate(address); err != nil {
		t.Fatal(err)
	}
	flags := txSendCmd.Flags()
	for name, value := range map[string]string{"to": string(address), "amount": "0.00000029"} {
		old := flags.Lookup(name).Value.String()
		t.Cleanup(func() { testutil.NoError(t, flags.Set(name, old)) })
		testutil.NoError(t, flags.Set(name, value))
	}
	var runErr error
	output := captureOutput(t, func() { runErr = txSendCmd.RunE(txSendCmd, nil) })
	if runErr == nil || !strings.Contains(runErr.Error(), "no transaction was submitted") ||
		strings.Contains(output, "Transaction submitted") || strings.Contains(output, "0x0000") {
		t.Fatalf("unsupported send reported success: err=%v output=%s", runErr, output)
	}
}

func TestPartialUnstake_ExactAmountReachesWire(t *testing.T) {
	amounts := make(chan json.Number, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/partial-unstake" {
			t.Errorf("unexpected request path %s", r.URL.Path)
		}
		var body struct {
			Amount json.Number `json:"amount_napr"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		amounts <- body.Amount
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	flags := validatorPartialUnstakeCmd.Flags()
	for name, value := range map[string]string{"node": server.URL, "pub-key": strings.Repeat("ab", 32), "amount": "0.00000029"} {
		old := flags.Lookup(name).Value.String()
		t.Cleanup(func() { testutil.NoError(t, flags.Set(name, old)) })
		testutil.NoError(t, flags.Set(name, value))
	}
	var runErr error
	captureOutput(t, func() { runErr = validatorPartialUnstakeCmd.RunE(validatorPartialUnstakeCmd, nil) })
	var amount json.Number
	select {
	case amount = <-amounts:
	default:
		t.Fatal("the command did not reach the request serialization control")
	}
	if runErr != nil || amount.String() != "29" {
		t.Fatalf("exact amount was changed: wire=%s err=%v", amount, runErr)
	}
}
