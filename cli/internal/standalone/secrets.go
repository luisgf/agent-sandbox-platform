package standalone

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
// empty). Two processes starting together agree: the file is created exclusively, and the one
// that loses reads the winner's.
func ReadOrCreate(path string, perm os.FileMode, gen func() string) (value string, created bool, err error) {
	for attempt := 0; attempt < 3; attempt++ {
		if b, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v, false, nil
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", false, err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		v := gen()
		_, werr := f.WriteString(v + "\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(path)
			return "", false, werr
		}
		return v, true, nil
	}
	return "", false, fmt.Errorf("%s: another process keeps making it", path)
}
