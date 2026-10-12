package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
)

// This is an operator declaration, not a signed activation configuration.
type operatorClaims struct {
	Scope         string `json:"scope"`
	GuardianTotal string `json:"guardian_total_napro"`
	Additional    string `json:"additional_off_registry_napro"`
}

func exactAmount(value string) (uint64, error) {
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != value {
		return 0, fmt.Errorf("claims require canonical unsigned decimal strings")
	}
	return n, nil
}

func loadClaims(path string) (*operatorClaims, error) {
	var claims operatorClaims
	if err := decodeDeclaration(path, &claims); err != nil {
		return nil, err
	}
	return &claims, nil
}

func decodeDeclaration(path string, target any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return fmt.Errorf("declaration must be a regular file of at most 4096 bytes")
	}
	decoder := json.NewDecoder(io.LimitReader(f, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("declaration contains trailing data")
	}
	return nil
}
