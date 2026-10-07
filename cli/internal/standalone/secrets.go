package standalone

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RandomHex is n random bytes as hex.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("standalone: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// ReadOrCreate returns what the file holds, making it with gen first if it is not there (or is
// empty). Two processes starting together agree: the value is written whole to a file of its own and
// linked to path, which fails if path exists, so one wins and the file is complete the moment it
// appears; the one that loses reads the winner's. (Creating path first and writing after would let
// the loser find it empty, take it for broken and make another.)
func ReadOrCreate(path string, perm os.FileMode, gen func() string) (value string, created bool, err error) {
	for attempt := 0; attempt < 3; attempt++ {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			if v := strings.TrimSpace(string(b)); v != "" {
				return v, false, nil
			}
			// Empty: not made by this function, which never leaves a file half written.
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", false, err
			}
		case !errors.Is(err, fs.ErrNotExist):
			return "", false, err
		}
		v := gen()
		won, err := publish(path, perm, v)
		if err != nil {
			return "", false, err
		}
		if won {
			return v, true, nil
		}
	}
	return "", false, fmt.Errorf("%s: another process keeps making it", path)
}

// publish makes path hold v, with mode perm, unless it exists: it reports whether it was this call
// that made it.
func publish(path string, perm os.FileMode, v string) (bool, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
	}
	name := tmp.Name()
	defer os.Remove(name) // the name only: once linked, the file stays reachable through path
	_, err = tmp.WriteString(v + "\n")
	if err == nil {
		err = tmp.Chmod(perm)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}
	if err := os.Link(name, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
