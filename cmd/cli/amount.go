package main

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

var aproAmountSyntax = regexp.MustCompile(`^\+?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE]([+-]?[0-9]+))?$`)

// parseAPROAmount preserves decimal amounts exactly, including bounded decimal
// exponent notation previously accepted by the CLI. It never passes through a
// float, truncates a fractional nAPRO, or accepts a non-finite value.
func parseAPROAmount(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 256 {
		return 0, fmt.Errorf("amount must be a bounded decimal APRO value")
	}
	match := aproAmountSyntax.FindStringSubmatch(raw)
	if match == nil {
		return 0, fmt.Errorf("amount must be a positive decimal APRO value")
	}
	if match[1] != "" {
		exponent, err := strconv.ParseInt(match[1], 10, 16)
		if err != nil || exponent < -256 || exponent > 256 {
			return 0, fmt.Errorf("amount exponent is outside the supported range")
		}
	}
	amount, ok := new(big.Rat).SetString(raw)
	if !ok || amount.Sign() <= 0 {
		return 0, fmt.Errorf("amount must be positive")
	}
	amount.Mul(amount, big.NewRat(100_000_000, 1))
	if !amount.IsInt() || !amount.Num().IsUint64() {
		return 0, fmt.Errorf("amount must be an exact positive uint64 number of nAPRO")
	}
	return amount.Num().Uint64(), nil
}

func formatAPROAmount(napro uint64) string {
	return fmt.Sprintf("%d.%08d", napro/100_000_000, napro%100_000_000)
}
