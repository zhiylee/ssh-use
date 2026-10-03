package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/zhiylee/ssh-use/internal/paths"
	"golang.org/x/sys/unix"
)

// Local and remote instances must not independently execute against the same
// audit database or mark one another's live commands as interrupted.
func lockInstance() (func(), error) {
	if err := os.MkdirAll(paths.DataDir(), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(paths.DataDir(), "instance.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another ssh-use daemon/server is using %s", paths.DataDir())
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}
