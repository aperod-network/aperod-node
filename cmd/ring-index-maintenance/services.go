package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aperod/aperod/store"
)

type serviceController struct{}

func systemctl(args ...string) (string, error) {
	if err := protectedPath("/usr/bin/systemctl", 0); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("systemctl operation failed: %w", err)
	}
	if out.Len() > 65536 {
		return "", fmt.Errorf("unexpected systemctl output size")
	}
	return strings.TrimSpace(out.String()), nil
}

func property(unit, key string) (string, error) {
	return systemctl("show", unit, "--property="+key, "--value")
}

func approvedUnitFiles(p maintenancePolicy) error {
	for _, u := range allUnits(p) {
		for path, want := range u.Files {
			if strings.ContainsAny(path, " \t\r\n\\\"") {
				return fmt.Errorf("ambiguous unit-file path")
			}
			if err := protectedPath(path, 0); err != nil {
				return err
			}
			got, err := fileDigest(path)
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("unit bytes differ from separate tool approval")
			}
		}
	}
	return nil
}

func allUnits(p maintenancePolicy) []unitPolicy {
	return append(append(append([]unitPolicy{}, p.Clients...), p.Writers...), p.Legacy)
}

func blockerDropIn(u unitPolicy, control string, invert bool) error {
	path := filepath.Join("/etc/systemd/system", u.Name+".d", "zzzz-aperod-ring-maintenance.conf")
	if _, ok := u.Files[path]; !ok {
		return fmt.Errorf("independently approved persistent service interlock missing")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	condition := control
	if invert {
		condition = "!" + control
	}
	want := "[Unit]\nConditionPathExists=" + condition + "\n"
	if string(body) != want {
		return fmt.Errorf("service interlock bytes do not bind approved database scope")
	}
	return nil
}

func (serviceController) validate(p maintenancePolicy, reload bool) error {
	if err := approvedUnitFiles(p); err != nil {
		return err
	}
	hold, err := store.RingMaintenanceControlPath(p.DBPath, "quiesced")
	if err != nil {
		return err
	}
	allow, err := store.RingMaintenanceControlPath(p.DBPath, "legacy-authorized")
	if err != nil {
		return err
	}
	for _, u := range append(append([]unitPolicy{}, p.Clients...), p.Writers...) {
		if err := blockerDropIn(u, hold, true); err != nil {
			return err
		}
	}
	if err := blockerDropIn(p.Legacy, allow, false); err != nil {
		return err
	}
	if reload {
		if _, err := systemctl("daemon-reload"); err != nil {
			return err
		}
	}
	for _, u := range allUnits(p) {
		fragment, err := property(u.Name, "FragmentPath")
		if err != nil {
			return err
		}
		dropins, err := property(u.Name, "DropInPaths")
		if err != nil {
			return err
		}
		actual := map[string]bool{fragment: true}
		for _, path := range strings.Fields(dropins) {
			if filepath.Base(path) > "zzzz-aperod-ring-maintenance.conf" {
				return fmt.Errorf("later drop-in can override maintenance condition")
			}
			actual[path] = true
		}
		if fragment == "" || len(actual) != len(u.Files) {
			return fmt.Errorf("runtime unit file set not approved")
		}
		for path := range actual {
			if _, ok := u.Files[path]; !ok {
				return fmt.Errorf("unapproved runtime unit override")
			}
		}
	}
	for key, want := range map[string]string{"User": "aperod", "DynamicUser": "no", "PrivateNetwork": "yes", "KillMode": "control-group", "Restart": "no", "Type": "oneshot", "RemainAfterExit": "no", "NoNewPrivileges": "yes", "Delegate": "no"} {
		got, err := property(p.Legacy.Name, key)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("dedicated legacy worker isolation/ownership not approved")
		}
	}
	enabled, err := property(p.Legacy.Name, "UnitFileState")
	if err != nil {
		return err
	}
	if enabled != "static" && enabled != "disabled" {
		return fmt.Errorf("legacy worker must not auto-start at boot")
	}
	if err := protectedPath(p.LegacyBinary, 0); err != nil {
		return err
	}
	digest, err := fileDigest(p.LegacyBinary)
	if err != nil {
		return err
	}
	if digest != p.LegacySHA256 {
		return fmt.Errorf("legacy worker binary not approved")
	}
	return nil
}

func (serviceController) stopped(u unitPolicy) error {
	state, err := property(u.Name, "ActiveState")
	if err != nil {
		return err
	}
	pid, err := property(u.Name, "MainPID")
	if err != nil {
		return err
	}
	pidEmptyOK := !strings.HasSuffix(u.Name, ".service") && pid == ""
	if (state != "inactive" && state != "failed") || (pid != "0" && !pidEmptyOK) {
		return fmt.Errorf("unit not quiescent")
	}
	job, err := property(u.Name, "Job")
	if err != nil {
		return err
	}
	if job != "" && job != "0" {
		return fmt.Errorf("pending unit job prevents quiescence")
	}
	group, err := property(u.Name, "ControlGroup")
	if err != nil {
		return err
	}
	if group == "" {
		return nil
	}
	if !strings.HasPrefix(group, "/") || group == "/" || filepath.Clean(group) != group {
		return fmt.Errorf("invalid runtime cgroup scope")
	}
	return emptyCgroup(filepath.Join("/sys/fs/cgroup", group))
}

func emptyCgroup(path string) error {
	procs, err := os.Open(filepath.Join(path, "cgroup.procs"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(procs, 65537))
	procs.Close()
	if err != nil {
		return err
	}
	if len(data) > 65536 || strings.TrimSpace(string(data)) != "" {
		return fmt.Errorf("writer cgroup still contains processes")
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		entries, err := dir.ReadDir(256)
		if err != nil && err != io.EOF {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				if err := emptyCgroup(filepath.Join(path, entry.Name())); err != nil {
					return err
				}
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

func (serviceController) activeBefore(p maintenancePolicy) ([]string, error) {
	var names []string
	for _, u := range append(append([]unitPolicy{}, p.Clients...), p.Writers...) {
		state, err := property(u.Name, "ActiveState")
		if err != nil {
			return nil, err
		}
		switch state {
		case "active":
			names = append(names, u.Name)
		case "inactive", "failed":
		default:
			return nil, fmt.Errorf("unit transition in progress; prepare refused")
		}
	}
	return names, nil
}

func previouslyActive(units []unitPolicy, names []string) []unitPolicy {
	selected := make(map[string]bool, len(names))
	for _, name := range names {
		selected[name] = true
	}
	var result []unitPolicy
	for _, u := range units {
		if selected[u.Name] {
			result = append(result, u)
		}
	}
	return result
}

func (s serviceController) stop(units []unitPolicy) error {
	for _, u := range units {
		if _, err := systemctl("stop", u.Name); err != nil {
			return err
		}
		if err := s.stopped(u); err != nil {
			return err
		}
	}
	return nil
}

func (serviceController) start(units []unitPolicy) error {
	for _, u := range units {
		if _, err := systemctl("start", u.Name); err != nil {
			return err
		}
	}
	return nil
}

// A running writer's actual LOCK descriptor witnesses scope without reading
// credentials/environment or trusting a guessed/default configuration path.
func (serviceController) witnessDB(p maintenancePolicy) error {
	if p.AllowColdPrepare {
		return nil
	} // must be separately approved in policy
	expected, err := fileDigest(p.NodeBinary)
	if err != nil {
		return err
	}
	for _, u := range p.Writers {
		text, err := property(u.Name, "MainPID")
		if err != nil {
			return err
		}
		pid, err := strconv.ParseUint(text, 10, 32)
		if err != nil {
			return err
		}
		if pid == 0 {
			continue
		}
		dir := filepath.Join("/proc", text, "fd")
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			if target == filepath.Join(p.DBPath, "LOCK") {
				image, err := os.Open(filepath.Join("/proc", text, "exe"))
				if err != nil {
					return err
				}
				h := sha256.New()
				_, err = io.Copy(h, image)
				image.Close()
				if err != nil {
					return err
				}
				if hex.EncodeToString(h.Sum(nil)) != expected {
					return fmt.Errorf("running image differs from backup binary")
				}
				return nil
			}
		}
	}
	return fmt.Errorf("live writer database scope unproven; explicit cold-copy approval required")
}
