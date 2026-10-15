package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/aperod/aperod/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ring maintenance refused:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("ring-index-maintenance", flag.ContinueOnError)
	phase := flags.String("phase", "", "prepare, legacy, finalize or guard-update")
	workerMode := flags.String("worker", "", "internal inherited-capability worker")
	db := flags.String("db", "", "internal approved database scope")
	generation := flags.String("generation", "", "internal receipt generation")
	candidate := flags.String("candidate", "", "already source-approved node candidate")
	current := flags.String("current", "", "current/rollback node executable")
	config := flags.String("config", "", "approved node configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *workerMode != "" {
		if *phase != "" || *candidate != "" || *current != "" || *config != "" {
			return fmt.Errorf("mixed worker/controller mode")
		}
		return worker(*workerMode, *db, *generation)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("separately authorized root coordinator required")
	}
	if *db != "" || *generation != "" {
		return fmt.Errorf("runtime scope overrides forbidden")
	}
	p, err := loadApprovedPolicy()
	if err != nil {
		return err
	}
	if *phase == "guard-update" {
		if *config != p.ConfigPath || *current == "" || *candidate == "" {
			return fmt.Errorf("update scope/arguments not approved")
		}
		if err := store.CheckRingMaintenanceFence(p.DBPath); err != nil {
			return err
		}
		if err := approvedIndexAware(p, *current); err != nil {
			return err
		}
		return approvedIndexAware(p, *candidate)
	}
	if *candidate != "" || *current != "" || *config != "" {
		return fmt.Errorf("unexpected update arguments")
	}
	if *phase != "prepare" && *phase != "legacy" && *phase != "finalize" {
		return fmt.Errorf("explicit supported phase required")
	}
	data, err := os.Stat(p.DataDir)
	if err != nil || !data.IsDir() {
		return fmt.Errorf("approved full data directory absent")
	}
	if resolved, err := filepathCanonical(p.DBPath); err != nil || resolved != p.DBPath {
		return fmt.Errorf("canonical DB path changed")
	}
	if err := protectedPath(p.NodeBinary, 0); err != nil {
		return err
	}
	s := serviceController{}
	if err := s.validate(p, true); err != nil {
		return err
	}
	l, err := store.AcquireRingMaintenance(p.DBPath)
	if err != nil {
		return err
	}
	defer l.Close()
	switch *phase {
	case "prepare":
		return prepare(p, l, s)
	case "legacy":
		return enterLegacy(p, l, s)
	case "finalize":
		return finalize(p, l, s)
	}
	return fmt.Errorf("unreachable phase")
}
