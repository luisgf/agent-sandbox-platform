package standalone

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"strconv"
)

// account is a user of this host the control plane can run as.
type account struct {
	name     string
	uid, gid int
}

// lookupUser finds a user by name; ok is false when there is none.
func lookupUser(name string) (account, bool) {
	if name == "" {
		return account{}, false
	}
	u, err := user.Lookup(name)
	if err != nil {
		return account{}, false
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return account{}, false
	}
	return account{name: name, uid: uid, gid: gid}, true
}

// lookupGroup finds a group by name.
func lookupGroup(name string) (gid int, ok bool) {
	if name == "" {
		return 0, false
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, false
	}
	gid, err = strconv.Atoi(g.Gid)
	return gid, err == nil
}

// isRoot reports whether this process can change owners and run children as another user.
func isRoot() bool { return os.Geteuid() == 0 }

// chown changes the owner of path; a process that is not root cannot, and leaves things as they
// are (everything is then its own).
func chown(path string, uid, gid int) error {
	if !isRoot() {
		return nil
	}
	err := os.Chown(path, uid, gid)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
