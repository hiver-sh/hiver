package isolation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// nssCANickname labels the sandbox CA entry in the workload's NSS database.
const nssCANickname = "Hiver Sandbox CA"

// buildNSSDB creates an NSS sql database trusting certPEM as a TLS CA and
// returns its files (cert9.db/key9.db/pkcs11.txt) keyed by base name. NSS
// clients (Chromium/Playwright) read neither the system bundle nor
// NODE_EXTRA_CA_CERTS — only the per-user db at $HOME/.pki/nssdb — so this is
// what makes them trust sbxproxy's minted leaf certs.
//
// The db is host- and arch-independent, so the host builds it once with the
// core image's certutil and the caller copies the files into any workload's
// nssdb: the merged rootfs for the container backend, or the params drive for
// the microvm guest (which then has no need for NSS tooling of its own).
func buildNSSDB(certPEM []byte) (map[string][]byte, error) {
	certutil, err := exec.LookPath("certutil")
	if err != nil {
		return nil, fmt.Errorf("certutil not found: %w", err)
	}
	dir, err := os.MkdirTemp("", "sbx-nssdb-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	cert, err := os.CreateTemp("", "sandbox-ca-*.pem")
	if err != nil {
		return nil, err
	}
	defer os.Remove(cert.Name())
	if _, err := cert.Write(certPEM); err != nil {
		return nil, err
	}
	_ = cert.Close()

	// -A adds a cert; "C,," trusts it as a CA for TLS; -a reads PEM. certutil
	// creates the sql cert9.db/key9.db in the empty dir on first write.
	cmd := exec.Command(certutil, "-d", "sql:"+dir, "-A",
		"-n", nssCANickname, "-t", "C,,", "-a", "-i", cert.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("certutil: %w (%s)", err, out)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files[e.Name()] = b
	}
	return files, nil
}

// writeNSSDB writes the files from [buildNSSDB] into dir, creating it. dir is
// the absolute $HOME/.pki/nssdb path (already prefixed with the rootfs for the
// container backend).
func writeNSSDB(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}
