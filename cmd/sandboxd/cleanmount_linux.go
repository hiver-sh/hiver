//go:build linux

package main

import (
	"bufio"
	"os"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// clearStaleMount unwedges a path left behind by a prior sandbox whose teardown
// didn't fully clear it. A FUSE mountpoint whose serving daemon has gone leaves
// a dead transport endpoint there: stat/mkdir/rmdir on it fail (ENOTCONN/EEXIST/
// EBUSY), so MkdirAll and RemoveAll both choke. Lazy-unmount (MNT_DETACH) every
// mount at or under path — detaching even a "busy" one — deepest first so nested
// mounts go before their parents, then RemoveAll the now-plain directory tree.
//
// path may be the mountpoint itself (start() recovery for a re-created key) or a
// parent dir holding nested mountpoints (teardown of keyRoot), so the mounts are
// discovered from /proc/self/mountinfo rather than assumed.
func clearStaleMount(path string) error {
	for _, mp := range mountsUnder(path) {
		_ = unix.Unmount(mp, unix.MNT_DETACH)
	}
	return os.RemoveAll(path)
}

// mountsUnder returns the mountpoints at or under path (deepest first) from
// /proc/self/mountinfo. Returns nil if mountinfo can't be read — the caller's
// RemoveAll still runs as a best-effort fallback.
func mountsUnder(path string) []string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()

	var found []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		// mountinfo field 5 (0-indexed 4) is the mount point, octal-escaped.
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMountField(fields[4])
		if mp == path || strings.HasPrefix(mp, path+"/") {
			found = append(found, mp)
		}
	}
	// Deepest first so a child unmounts before its parent.
	sort.Slice(found, func(i, j int) bool { return len(found[i]) > len(found[j]) })
	return found
}

// unescapeMountField decodes the octal escapes (\040 space, \011 tab, \012
// newline, \134 backslash) the kernel writes for special characters in
// mountinfo paths.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if c, ok := octalByte(s[i+1], s[i+2], s[i+3]); ok {
				b.WriteByte(c)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func octalByte(a, b, c byte) (byte, bool) {
	if a < '0' || a > '7' || b < '0' || b > '7' || c < '0' || c > '7' {
		return 0, false
	}
	return (a-'0')<<6 | (b-'0')<<3 | (c - '0'), true
}
