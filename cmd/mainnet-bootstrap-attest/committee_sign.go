package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/mainnetbootstrap"
)

// The command accepts no custom statement: its authority is deliberately
// limited to the reviewed Mainnet historical committee.
func runCommitteeSign(args []string) error {
	flags := flag.NewFlagSet("committee-sign", flag.ContinueOnError)
	path := flags.String("key", "", "path to the local mode-0600 Mainnet operator key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return errors.New("committee-sign requires only --key")
	}
	key, err := loadOperatorKey(*path)
	if err != nil {
		return err
	}
	defer key.Destroy()
	statement := mainnetbootstrap.HistoricalCommitteeDraft()
	pin := statement.Approvals[0].Validator
	if !key.Public().Equals(pin) {
		return errors.New("provided key does not match the pinned Mainnet operator")
	}
	message, err := statement.SigningMessage()
	if err != nil {
		return err
	}
	statement.Approvals[0].Signature, err = key.Sign(message)
	if err != nil {
		return err
	}
	if err := mainnetbootstrap.VerifyHistoricalCommitteeOperator(statement, []crypto.ValidatorPubKey{pin}); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(statement)
}
