package main

// Auth-state persistence. Chrome's user-data-dir (profileDir) is ephemeral — it
// lives on the VM overlay and is wiped on each boot. To keep the browser signed
// in to sites across tasks and VM recreates, we persist ONLY its durable,
// auth-bearing state to a small GCS-backed mount (stateDir), keyed per user data
// location by the app (see browserStateFs in work/lib/hiver/gcs.ts). Chrome's
// caches (HTTP cache, code cache, GPU/shader caches, service-worker CacheStorage
// — the bulk of the profile) are deliberately NOT synced: routing that churn
// through GCS would be far too costly. So we copy a fixed allowlist into the
// profile on boot (before Chrome launches) and back out on exit (after Chrome
// has cleanly closed its SQLite databases), never a live mount over the profile.

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// saveMu serializes saveAuthState so the periodic ticker and the final post-exit
// save can't interleave writes to the same destination files.
var saveMu sync.Mutex

// stateDir is the GCS-backed mount the app attaches at BROWSER_STATE_DIR. When
// no bucket is configured the dir is simply absent and every sync below no-ops.
func stateDir() string { return env("HIVER_CHROME_STATE_DIR", "/opt/hiver/chrome-state") }

// authFiles are the loose profile files (relative to profileDir) that carry site
// auth. Each is a SQLite database, so its journal/WAL sidecars ride along (see
// sqliteSidecars). This build keeps cookies at "Default/Cookies" (a loose file,
// not "Default/Network/Cookies"); "Login Data"/"Login Data For Account" are the
// saved-password stores and "Web Data" the autofill/form store. passkeys.json is
// chromehost's own WebAuthn store at the profile root.
var authFiles = []string{
	"Default/Cookies",
	"Default/Login Data",
	"Default/Login Data For Account",
	"Default/Web Data",
	"passkeys.json",
}

// sqliteSidecars are the companions synced next to each authFiles SQLite DB. The
// -shm shared-memory file is intentionally omitted: it is rebuildable and copying
// a stale one can confuse WAL recovery — SQLite recreates it from the DB + -wal.
var sqliteSidecars = []string{"", "-journal", "-wal"}

// authDirs are the profile subdirectories (relative to profileDir) that carry
// site auth as LevelDB/blob stores. "Default/Network" is included so cookies are
// still captured on Chromium builds that keep them there (plus its small network
// state); on this build it's simply absent and skipped.
var authDirs = []string{
	"Default/Network",
	"Default/Local Storage",
	"Default/Session Storage",
	"Default/IndexedDB",
}

// restoreAuthState copies the persisted auth state from stateDir into the profile
// before Chrome launches, so it starts already signed in. Chrome is not yet
// running, so the source files are quiescent. A missing stateDir (no bucket, or
// first ever run) is a silent no-op.
func restoreAuthState() {
	sd := stateDir()
	if _, err := os.Stat(sd); err != nil {
		return
	}
	syncAuthState(sd, profileDir, "restore")
}

// saveAuthState copies the auth state from the profile back to stateDir. The
// authoritative call is after Chrome exits (its SQLite DBs are checkpointed and
// closed, so the copy is consistent); the periodic call while Chrome runs is a
// best-effort safety net for an abrupt VM stop. A missing stateDir is a no-op.
func saveAuthState() {
	sd := stateDir()
	if _, err := os.Stat(sd); err != nil {
		return
	}
	saveMu.Lock()
	defer saveMu.Unlock()
	syncAuthState(profileDir, sd, "save")
}

func syncAuthState(srcRoot, dstRoot, direction string) {
	for _, rel := range authFiles {
		for _, sc := range sqliteSidecars {
			r := rel + sc
			if err := copyPath(filepath.Join(srcRoot, r), filepath.Join(dstRoot, r)); err != nil && !os.IsNotExist(err) {
				log.Printf("authstate %s %q: %v", direction, r, err)
			}
		}
	}
	for _, rel := range authDirs {
		if err := copyPath(filepath.Join(srcRoot, rel), filepath.Join(dstRoot, rel)); err != nil && !os.IsNotExist(err) {
			log.Printf("authstate %s %q: %v", direction, rel, err)
		}
	}
}

// copyPath copies a regular file or a directory tree from src to dst. A missing
// src returns an fs.ErrNotExist error the caller treats as "nothing to sync".
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyDir(src, dst)
	}
	return copyFile(src, dst, info.Mode())
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		// Skip lock files, sockets, and other non-regular files (e.g. LevelDB's
		// LOCK) — they are process-local and must not be carried across VMs.
		if !info.Mode().IsRegular() {
			continue
		}
		if err := copyFile(s, d, info.Mode()); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies one regular file, truncating any existing destination. It
// writes straight to dst (no temp+rename): the only reader is the next boot's
// restore or a later save, never a concurrent one, and rename semantics over the
// GCS-backed mount are best avoided.
func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
