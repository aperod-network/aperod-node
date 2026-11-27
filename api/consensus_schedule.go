package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
)

const maxConsensusMigrationWitnessBytes = 64 << 20

var errInvalidConsensusMigrationWitness = errors.New("invalid consensus migration witness")

type consensusScheduleSnapshot struct {
	guardianFundActivationHeight    uint64
	lpodDailyUpgradeHeight          uint64
	lpodValidatorDailyUpgradeHeight uint64
	migrationWitnessSHA256          string
	migrationWitnessStatus          string
}

// SetConsensusActivationSchedule records the consensus heights and snapshots
// the configured migration witness digest for the private operator endpoint.
// Call before Start. The witness path is never included in API responses.
func (s *Server) SetConsensusActivationSchedule(
	guardianFundActivationHeight uint64,
	lpodDailyUpgradeHeight uint64,
	lpodValidatorDailyUpgradeHeight uint64,
	migrationWitnessPath string,
) {
	snapshot := consensusScheduleSnapshot{
		guardianFundActivationHeight:    guardianFundActivationHeight,
		lpodDailyUpgradeHeight:          lpodDailyUpgradeHeight,
		lpodValidatorDailyUpgradeHeight: lpodValidatorDailyUpgradeHeight,
		migrationWitnessStatus:          "not_configured",
	}
	if migrationWitnessPath != "" {
		digest, err := consensusMigrationWitnessDigest(migrationWitnessPath)
		if err != nil {
			snapshot.migrationWitnessStatus = "unavailable"
		} else {
			snapshot.migrationWitnessSHA256 = digest
			snapshot.migrationWitnessStatus = "available"
		}
	}
	s.consensusSchedule = snapshot
}

func consensusMigrationWitnessDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxConsensusMigrationWitnessBytes {
		return "", errInvalidConsensusMigrationWitness
	}

	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, maxConsensusMigrationWitnessBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxConsensusMigrationWitnessBytes {
		return "", errInvalidConsensusMigrationWitness
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *Server) restConsensusSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}

	// This is only the schedule loaded by this node; it does not attest that
	// other validators use the same values.
	w.Header().Set("Cache-Control", "no-store")
	snapshot := s.consensusSchedule
	status := snapshot.migrationWitnessStatus
	if status == "" {
		status = "not_configured"
	}
	var digest interface{}
	if snapshot.migrationWitnessSHA256 != "" {
		digest = snapshot.migrationWitnessSHA256
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"guardian_fund_activation_height":     snapshot.guardianFundActivationHeight,
		"lpod_daily_upgrade_height":           snapshot.lpodDailyUpgradeHeight,
		"lpod_validator_daily_upgrade_height": snapshot.lpodValidatorDailyUpgradeHeight,
		"migration_witness_status":            status,
		"migration_witness_sha256":            digest,
	})
}
