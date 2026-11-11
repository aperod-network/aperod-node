package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExplicitMaintenanceDataDir(t *testing.T) {
	for _, command := range []string{"--compact-db", "--repair-height-index"} {
		t.Run(command, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				args    []string
				want    string
				wantErr string
			}{
				{name: "missing", wantErr: "requires --data-dir=<path>"},
				{name: "config is not a data directory", args: []string{"--config", "/etc/aperod/node.yaml"}, wantErr: "requires --data-dir=<path>"},
				{name: "missing separate value", args: []string{"--data-dir"}, wantErr: "requires a path"},
				{name: "next flag is not a path", args: []string{"--data-dir", "--repair-db"}, wantErr: "requires a path"},
				{name: "empty separate value", args: []string{"--data-dir", ""}, wantErr: "must not be empty"},
				{name: "empty equals value", args: []string{"--data-dir="}, wantErr: "must not be empty"},
				{name: "whitespace is not a path", args: []string{"--data-dir= \t"}, wantErr: "must not be empty"},
				{name: "conflicting duplicate", args: []string{"--data-dir=/first", "--data-dir", "/second"}, wantErr: "exactly once"},
				{name: "identical duplicate", args: []string{"--data-dir", "/first", "--data-dir=/first"}, wantErr: "exactly once"},
				{name: "empty duplicate cannot override target", args: []string{"--data-dir=/first", "--data-dir="}, wantErr: "exactly once"},
				{name: "equals", args: []string{"--data-dir=/chosen/chain"}, want: "/chosen/chain"},
				{name: "separate", args: []string{"--data-dir", "/chosen/chain"}, want: "/chosen/chain"},
				{name: "relative path", args: []string{"--data-dir=./chosen"}, want: "./chosen"},
				{name: "space preserved", args: []string{"--data-dir", "/chosen/ chain "}, want: "/chosen/ chain "},
				{name: "before command", args: []string{"--data-dir", "/chosen", command}, want: "/chosen"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					args := append([]string{command}, tc.args...)
					got, err := explicitMaintenanceDataDir(args, command)
					if tc.wantErr != "" {
						if err == nil || !strings.Contains(err.Error(), command) || !strings.Contains(err.Error(), tc.wantErr) {
							t.Fatalf("got path %q, error %v; want command-specific error containing %q", got, err, tc.wantErr)
						}
						if got != "" {
							t.Errorf("invalid arguments exposed a target %q", got)
						}
						return
					}
					if err != nil || got != tc.want {
						t.Fatalf("got (%q, %v), want (%q, nil)", got, err, tc.want)
					}
				})
			}
		})
	}
}

func TestEarlyMaintenanceCommandsRejectAmbiguousTargetsBeforeOpeningDB(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("APEROD_DATA_DIR", filepath.Join(root, "chosen"))
	before := make(map[string]map[string][32]byte)
	for _, name := range []string{"data", "--repair-db", "chosen", "other"} {
		dataDir := filepath.Join(root, name)
		createMaintenanceTestDB(t, dataDir, 100)
		before[dataDir] = maintenanceDBState(t, dataDir)
	}
	for _, command := range []string{"--compact-db", "--repair-height-index"} {
		for _, tc := range []struct {
			name string
			args []string
			want string
		}{
			{name: "no target", want: "requires --data-dir=<path>"},
			{name: "no value", args: []string{"--data-dir"}, want: "requires a path"},
			{name: "empty value", args: []string{"--data-dir="}, want: "must not be empty"},
			{name: "flag names an existing directory", args: []string{"--data-dir", "--repair-db"}, want: "requires a path"},
			{name: "two existing targets", args: []string{"--data-dir=chosen", "--data-dir=other"}, want: "exactly once"},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				oldArgs := os.Args
				os.Args = append([]string{"aperod-node", command}, tc.args...)
				t.Cleanup(func() { os.Args = oldArgs })
				err := run()
				if err == nil || !strings.Contains(err.Error(), command) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("wanted target-argument refusal %q, got %v", tc.want, err)
				}
				for dataDir, state := range before {
					if after := maintenanceDBState(t, dataDir); !reflect.DeepEqual(after, state) {
						t.Errorf("invalid maintenance arguments changed %s", dataDir)
					}
				}
			})
		}
	}
}
