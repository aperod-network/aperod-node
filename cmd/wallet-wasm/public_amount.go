package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

const maxSafeJSONInteger = uint64(1<<53 - 1)

// parsePublicAmount accepts exact decimal strings up to uint64 and JSON
// numbers only when they are canonical integer tokens safely representable by
// JavaScript. It never converts through float64.
func parsePublicAmount(raw json.RawMessage) (uint64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0, fmt.Errorf("amount_napr must be a canonical decimal string or safe integer")
	}
	var digits string
	isString := raw[0] == '"'
	if isString {
		if err := json.Unmarshal(raw, &digits); err != nil {
			return 0, fmt.Errorf("amount_napr must be a canonical decimal string: %w", err)
		}
	} else {
		digits = string(raw)
		for _, r := range digits {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("numeric amount_napr must be an unsigned integer token")
			}
		}
	}
	if !canonicalDecimal(digits) {
		return 0, fmt.Errorf("amount_napr must use canonical unsigned decimal notation")
	}
	amount, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount_napr is outside the uint64 range")
	}
	if !isString && amount > maxSafeJSONInteger {
		return 0, fmt.Errorf("numeric amount_napr exceeds JavaScript's safe integer range; send it as a decimal string")
	}
	return amount, nil
}

func canonicalDecimal(s string) bool {
	if s == "0" {
		return true
	}
	if len(s) == 0 || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
