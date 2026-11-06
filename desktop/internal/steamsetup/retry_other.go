//go:build !windows

package steamsetup

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const noFollowFlag = syscall.O_NOFOLLOW

func transientOS(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK || errno == syscall.EBUSY || errno == syscall.EINTR
	}
	return false
}

// syncDir makes a rename or unlink durable; failure only weakens durability.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// checkDirTrusted refuses a hosts directory others can write to without the sticky bit, because
// they could swap the journal.
func checkDirTrusted(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o002 != 0 && st.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("hosts directory %s is writable by everyone", dir)
	}
	return nil
}
