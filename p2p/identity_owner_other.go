//go:build !linux

package p2p

import "os"

func validateP2PIdentityOwner(os.FileInfo) error {
	return nil
}
