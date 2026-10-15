//go:build linux

package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
	"golang.org/x/sys/unix"
)

type diskResources struct {
	RSSBytes     uint64 `json:"rss_bytes"`
	PeakRSSBytes uint64 `json:"peak_rss_bytes"`
}

func diskResourceSnapshot() (diskResources, error) {
	var r diskResources
	for _, input := range []struct {
		path string
		dest map[string]*uint64
	}{
		{"/proc/self/status", map[string]*uint64{"VmRSS:": &r.RSSBytes, "VmHWM:": &r.PeakRSSBytes}},
	} {
		raw, err := os.ReadFile(input.path)
		if err != nil {
			return r, err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			dest := input.dest[fields[0]]
			if dest == nil {
				continue
			}
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return r, err
			}
			if len(fields) > 2 && fields[2] == "kB" {
				value *= 1024
			}
			*dest = value
		}
	}
	return r, nil
}

type diskPhase struct {
	Name               string        `json:"name"`
	Seconds            float64       `json:"seconds"`
	Calls              int           `json:"calls,omitempty"`
	P50MS              float64       `json:"p50_ms,omitempty"`
	P95MS              float64       `json:"p95_ms,omitempty"`
	P99MS              float64       `json:"p99_ms,omitempty"`
	MaxMS              float64       `json:"max_ms,omitempty"`
	Start              diskResources `json:"start"`
	End                diskResources `json:"end"`
	ObservedRSSMax     uint64        `json:"observed_rss_max_bytes"`
	BackendReadBytes   uint64        `json:"leveldb_file_read_bytes_delta"`
	BackendWriteBytes  uint64        `json:"leveldb_file_write_bytes_delta"`
	BackendIOAvailable bool          `json:"phase_leveldb_io_available"`
	SampleReadBytes    uint64        `json:"sample_only_leveldb_file_read_bytes,omitempty"`
	SampleReadCalls    int           `json:"samples_with_leveldb_file_reads,omitempty"`
	ApparentBytes      int64         `json:"db_apparent_bytes"`
	AllocatedBytes     int64         `json:"db_allocated_bytes"`
}

func measureDiskPhase(name, dbPath string, database func() *DB, work func() ([]time.Duration, error)) (diskPhase, error) {
	r := diskPhase{Name: name}
	start, err := diskResourceSnapshot()
	if err != nil {
		return r, err
	}
	r.Start = start
	startDB := database()
	var ioStart leveldb.DBStats
	if err := startDB.db.Stats(&ioStart); err != nil {
		return r, err
	}
	var mu sync.Mutex
	maxRSS := start.RSSBytes
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if sample, err := diskResourceSnapshot(); err == nil {
					mu.Lock()
					if sample.RSSBytes > maxRSS {
						maxRSS = sample.RSSBytes
					}
					mu.Unlock()
				}
			}
		}
	}()
	begin := time.Now()
	durations, workErr := work()
	r.Seconds = time.Since(begin).Seconds()
	close(stop)
	<-done
	r.End, err = diskResourceSnapshot()
	if err != nil {
		return r, err
	}
	mu.Lock()
	r.ObservedRSSMax = maxRSS
	mu.Unlock()
	if r.End.RSSBytes > r.ObservedRSSMax {
		r.ObservedRSSMax = r.End.RSSBytes
	}
	if endDB := database(); endDB == startDB {
		var ioEnd leveldb.DBStats
		if err := endDB.db.Stats(&ioEnd); err != nil {
			return r, err
		}
		r.BackendIOAvailable = true
		r.BackendReadBytes = ioEnd.IORead - ioStart.IORead
		r.BackendWriteBytes = ioEnd.IOWrite - ioStart.IOWrite
	}
	r.Calls = len(durations)
	if len(durations) > 0 {
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		q := func(p float64) float64 {
			index := int(float64(len(durations)-1)*p + 0.5)
			return float64(durations[index]) / float64(time.Millisecond)
		}
		r.P50MS, r.P95MS, r.P99MS, r.MaxMS = q(.5), q(.95), q(.99), q(1)
	}
	err = filepath.Walk(dbPath, func(_ string, info os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if info.Mode().IsRegular() {
			r.ApparentBytes += info.Size()
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				r.AllocatedBytes += stat.Blocks * 512
			}
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	return r, workErr
}

// Local, advisory eviction only. A successful syscall is NOT proof of a fully
// cold host/storage cache. The report retains LevelDB file-read byte counts.
func adviseDiskFilesCold(path string) error {
	return filepath.Walk(path, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		defer file.Close()
		return unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_DONTNEED)
	})
}

func writeDiskReport(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".rehearsal-report-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync report: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync report directory: %w", err)
	}
	return nil
}

func TestRingDiskReportAtomicCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	for _, passed := range []bool{false, true} {
		if err := writeDiskReport(path, map[string]bool{"passed": passed}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]bool
		if err := json.Unmarshal(data, &got); err != nil || got["passed"] != passed {
			t.Fatal("invalid checkpoint", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary report files leaked", err)
	}
}
