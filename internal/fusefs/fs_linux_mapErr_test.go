//go:build linux

package fusefs

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestMapErr(t *testing.T) {
	tests := []struct {
		name string
		in   error
		want syscall.Errno
	}{
		{
			name: "PathError wrapping ENOENT returns ENOENT",
			in:   &os.PathError{Op: "remove", Path: "/f", Err: syscall.ENOENT},
			want: syscall.ENOENT,
		},
		{
			name: "PathError wrapping EPERM returns EACCES",
			in:   &os.PathError{Op: "open", Path: "/f", Err: syscall.EPERM},
			want: syscall.EACCES,
		},
		{
			name: "PathError wrapping ENOTEMPTY returns ENOTEMPTY not EIO",
			in:   &os.PathError{Op: "remove", Path: "/dir", Err: syscall.ENOTEMPTY},
			want: syscall.ENOTEMPTY,
		},
		{
			name: "PathError wrapping EEXIST returns EEXIST",
			in:   &os.PathError{Op: "mkdir", Path: "/dir", Err: syscall.EEXIST},
			want: syscall.EEXIST,
		},
		{
			name: "bare ENOTEMPTY returns ENOTEMPTY",
			in:   syscall.ENOTEMPTY,
			want: syscall.ENOTEMPTY,
		},
		{
			name: "bare ENOENT returns ENOENT",
			in:   syscall.ENOENT,
			want: syscall.ENOENT,
		},
		{
			name: "non-errno error returns EIO",
			in:   errors.New("unexpected"),
			want: syscall.EIO,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapErr(tc.in)
			var gotErrno syscall.Errno
			if !errors.As(got, &gotErrno) {
				t.Fatalf("mapErr(%v) = %v (%T), want a syscall.Errno", tc.in, got, got)
			}
			if gotErrno != tc.want {
				t.Errorf("mapErr(%v) = %v, want %v", tc.in, gotErrno, tc.want)
			}
		})
	}
}
