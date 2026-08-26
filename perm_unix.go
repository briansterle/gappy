//go:build unix

package main

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// setUmask sets the process file-creation mask, which is what actually decides
// the mode of files gappy doesn't create itself — go-containerregistry writes
// blobs and rewrites index.json with os.Create and os.ModePerm, and the umask
// is the only lever over those.
// It returns the previous mask, which is the only way to read one.
func setUmask(mask int) int { return syscall.Umask(mask) }

// fileOwner names the user owning fi, for telling someone exactly whose files
// are blocking a permission repair. Falls back to the bare uid when the name
// can't be resolved, which is normal on an NFS client without the directory.
func fileOwner(fi os.FileInfo) (uid string, name string) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ""
	}
	uid = strconv.FormatUint(uint64(st.Uid), 10)
	if u, err := user.LookupId(uid); err == nil {
		name = u.Username
	}
	return uid, name
}
