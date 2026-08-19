//go:build !linux && !darwin

package ports

import "errors"

// takePlatform has no implementation on this OS, so Take() degrades to dial
// probing: we can still say whether a port is occupied, just not by whom.
func takePlatform() (*Snapshot, error) {
	return nil, errors.New("port ownership lookup is not implemented on this platform")
}
