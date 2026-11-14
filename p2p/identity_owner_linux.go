//go:build linux

package p2p

import (
	"fmt"
	"os"
	"syscall"
)

func validateP2PIdentityOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot verify file owner")
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("must be owned by the node process user")
	}
	return nil
}
