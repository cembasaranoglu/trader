//go:build unix

package credentials

import (
	"errors"
	"os"
	"syscall"
)

// ErrCredentialsFileOwner is returned when a credentials or key file is not
// owned by the user running the process.
var ErrCredentialsFileOwner = errors.New("credentials: credentials and key files must be owned by the current user")

// checkPrivate requires owner-only permissions and ownership by the current user.
func checkPrivate(_ string, st os.FileInfo) ([]string, error) {
	if st.Mode().Perm()&0o077 != 0 {
		return nil, ErrInsecureCredentialsFile
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("credentials: cannot read file owner")
	}
	if int(sys.Uid) != os.Geteuid() {
		return nil, ErrCredentialsFileOwner
	}
	return nil, nil
}
