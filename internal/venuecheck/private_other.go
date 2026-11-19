//go:build !unix

package venuecheck

import "os"

// checkPrivate cannot verify access on this platform: Windows file access is
// governed by ACLs that Go's FileMode does not expose. It returns a warning so
// the gap is visible instead of silently passing.
func checkPrivate(path string, _ os.FileInfo) ([]string, error) {
	return []string{"file access (ACL) of " + path + " was not verified on this platform; make sure only your user can read it"}, nil
}
