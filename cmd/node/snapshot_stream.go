package main

import (
	"encoding/json"
	"fmt"
	"io"
)

// writeSnapshotArray encodes one bounded chunk at a time. json.Encoder.Encode
// buffers its entire argument before writing, which previously kept a second
// multi-gigabyte copy of the UTXO snapshot alive during gzip compression.
func writeSnapshotArray[T any](w io.Writer, values []T) error {
	if values == nil {
		_, err := io.WriteString(w, "null")
		return err
	}
	if _, err := io.WriteString(w, "["); err != nil {
		return err
	}
	const chunkSize = 256
	for start := 0; start < len(values); start += chunkSize {
		end := start + chunkSize
		if end > len(values) {
			end = len(values)
		}
		chunk, err := json.Marshal(values[start:end])
		if err != nil {
			return err
		}
		if start > 0 {
			if _, err = io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if _, err = w.Write(chunk[1 : len(chunk)-1]); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "]")
	return err
}

func writeSnapshotField(w io.Writer, name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%q:%s", name, raw)
	return err
}

// writeStartupSnapshotJSON preserves the v2 JSON schema while ensuring the
// encoder never constructs a second full-size JSON buffer. The detached
// UTXOSnapshot remains immutable until this function returns.
func writeStartupSnapshotJSON(w io.Writer, snap startupSnapshot) error {
	if _, err := io.WriteString(w, "{"); err != nil {
		return err
	}
	for i, field := range []struct {
		name  string
		value any
	}{
		{"v", snap.Version},
		{"tip_height", snap.TipHeight},
		{"tip_hash", snap.TipHashHex},
		{"tx_total", snap.TxTotal},
		{"saved_at", snap.SavedAt},
	} {
		if i > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if err := writeSnapshotField(w, field.name, field.value); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, `,"utxos":{`); err != nil {
		return err
	}
	for i, field := range []struct {
		name  string
		write func(io.Writer) error
	}{
		{"active_utxos", func(w io.Writer) error { return writeSnapshotArray(w, snap.UTXOs.ActiveUTXOs) }},
		{"staked_utxos", func(w io.Writer) error { return writeSnapshotArray(w, snap.UTXOs.StakedUTXOs) }},
		{"spent_decoys", func(w io.Writer) error { return writeSnapshotArray(w, snap.UTXOs.SpentDecoys) }},
		{"key_images", func(w io.Writer) error { return writeSnapshotArray(w, snap.UTXOs.KeyImages) }},
	} {
		if i > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if err := writeSnapshotFieldName(w, field.name); err != nil {
			return err
		}
		if err := field.write(w); err != nil {
			return err
		}
	}
	if len(snap.UTXOs.RollbackJournal) > 0 {
		if _, err := io.WriteString(w, ","); err != nil {
			return err
		}
		if err := writeSnapshotFieldName(w, "rollback_journal"); err != nil {
			return err
		}
		if err := writeSnapshotArray(w, snap.UTXOs.RollbackJournal); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, `},"registry":`); err != nil {
		return err
	}
	if err := writeSnapshotFieldValue(w, snap.Registry); err != nil {
		return err
	}
	_, err := io.WriteString(w, "}\n")
	return err
}

func writeSnapshotFieldName(w io.Writer, name string) error {
	_, err := fmt.Fprintf(w, "%q:", name)
	return err
}

func writeSnapshotFieldValue(w io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}
