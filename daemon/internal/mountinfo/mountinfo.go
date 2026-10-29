// Package mountinfo finds a mount of a given filesystem type in a
// /proc/self/mounts formatted file.
package mountinfo

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Find returns the first mount point of filesystem type fsType in the
// mounts file, or "" when there is none.
func Find(mountsPath, fsType string) (string, error) {
	f, err := os.Open(mountsPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 3 && fields[2] == fsType {
			return Unescape(fields[1]), nil
		}
	}
	return "", sc.Err()
}

// Wait returns explicit when set, else polls the mounts file for a mount of
// fsType until it appears or wait elapses.
func Wait(ctx context.Context, explicit, mountsPath, fsType string, wait time.Duration) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	deadline := time.Now().Add(wait)
	for {
		mnt, err := Find(mountsPath, fsType)
		if err == nil && mnt != "" {
			return mnt, nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("no %s mount in %s after %s", fsType, mountsPath, wait)
			}
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Unescape undoes the octal escapes (\040 for space, etc.) the kernel applies
// to paths in /proc/self/mounts.
func Unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
