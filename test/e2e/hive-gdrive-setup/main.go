// Command hive-gdrive-setup walks a user through obtaining the
// HIVE_GDRIVE_* env vars Hive expects when fs.backend is "gdrive".
//
// The flow:
//
//  1. Prompt for client_id + client_secret (from a Desktop OAuth
//     client at https://console.cloud.google.com/apis/credentials).
//  2. Open a localhost HTTP server, build the Google auth URL pointing
//     back at it, and ask the user to visit it.
//  3. Capture the redirect's auth code, exchange it for access +
//     refresh tokens.
//  4. List the user's top-level Drive folders so they can pick one to
//     scope the workspace to (or enter a custom folder id).
//  5. Print `export HIVE_GDRIVE_…=…` lines on stdout — everything
//     interactive goes to stderr, so a calling script can `eval` the
//     output and pick up the env vars.
//
// Usage from a shell:
//
//	eval "$(go run ./test/e2e/hive-gdrive-setup)"
//
// run-fixture.sh does this automatically when the fixture's spec.yaml
// has fs.backend == gdrive and HIVE_GDRIVE_ACCESS_TOKEN isn't set.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	in := bufio.NewReader(os.Stdin)

	infof("Hive — Google Drive OAuth setup")
	infof("")
	infof("If you don't have an OAuth client yet, create a 'Desktop app' OAuth 2.0")
	infof("Client ID at https://console.cloud.google.com/apis/credentials.")
	infof("")

	clientID, err := promptOrEnv(in, "HIVE_GDRIVE_CLIENT_ID", "Client ID: ")
	if err != nil {
		return err
	}
	clientSecret, err := promptOrEnv(in, "HIVE_GDRIVE_CLIENT_SECRET", "Client secret: ")
	if err != nil {
		return err
	}

	// Bind the OAuth callback. Default = OS-picked random port (works
	// for Desktop-app OAuth clients, which Google auto-trusts on
	// 127.0.0.1:*). Pin the port via HIVE_GDRIVE_REDIRECT_PORT when
	// using a Web-app client that has a specific redirect URI
	// registered — otherwise Google answers `redirect_uri_mismatch`
	// because the random port we'd pick isn't on its allowlist.
	listenAddr := "127.0.0.1:0"
	if p := os.Getenv("HIVE_GDRIVE_REDIRECT_PORT"); p != "" {
		listenAddr = "127.0.0.1:" + p
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s for redirect: %w", listenAddr, err)
	}
	redirectURI := fmt.Sprintf("http://%s/oauth/callback", listener.Addr().String())

	infof("")
	infof("Redirect URI:")
	infof("  %s", redirectURI)
	infof("")
	infof("If your OAuth client is type 'Desktop app', you're good — Google")
	infof("auto-accepts any 127.0.0.1:* URI. If it's 'Web application', this")
	infof("exact URI must be registered as an Authorized redirect URI on the")
	infof("client (https://console.cloud.google.com/apis/credentials). Pin")
	infof("the port with HIVE_GDRIVE_REDIRECT_PORT=<n> to keep it stable.")
	infof("")

	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{drive.DriveScope},
		RedirectURL:  redirectURI,
	}
	state := randomState()
	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"))

	infof("Open this URL in your browser to authorize Hive:")
	infof("  %s", authURL)
	infof("")
	openBrowser(authURL)
	infof("Waiting for callback on %s …", redirectURI)

	ctx := context.Background()
	code, err := waitForCode(ctx, listener, state)
	if err != nil {
		return err
	}
	infof("✓ Authorization code received")

	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	infof("✓ Access + refresh tokens obtained")
	infof("")

	httpClient := cfg.Client(ctx, tok)
	svc, err := drive.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return fmt.Errorf("drive client: %w", err)
	}

	folders, err := listFolders(ctx, svc)
	if err != nil {
		return fmt.Errorf("list folders: %w", err)
	}

	folderID, err := chooseFolder(in, folders)
	if err != nil {
		return err
	}

	emitExports(clientID, clientSecret, tok, folderID)
	return nil
}

func promptOrEnv(in *bufio.Reader, envKey, prompt string) (string, error) {
	if v := os.Getenv(envKey); v != "" {
		infof("Using %s from environment", envKey)
		return v, nil
	}
	fmt.Fprint(os.Stderr, prompt)
	line, err := in.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read %s: %w", envKey, err)
	}
	v := strings.TrimSpace(line)
	if v == "" {
		return "", fmt.Errorf("%s required", envKey)
	}
	return v, nil
}

func waitForCode(ctx context.Context, l net.Listener, state string) (string, error) {
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("state"); got != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("state mismatch: got %q, want %q", got, state)
			return
		}
		if errStr := q.Get("error"); errStr != "" {
			http.Error(w, "auth error: "+errStr, http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth: %s", errStr)
			return
		}
		code := q.Get("code")
		_, _ = fmt.Fprintln(w, "<!doctype html><meta charset=utf-8><title>Hive</title>"+
			"<body style='font-family:system-ui;padding:2rem;'>"+
			"<h2>✓ Hive authorization received</h2>"+
			"<p>You can close this tab and return to your terminal.</p></body>")
		codeCh <- code
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()
	select {
	case code := <-codeCh:
		return code, nil
	case err := <-errCh:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type folder struct {
	ID, Name string
}

func listFolders(ctx context.Context, svc *drive.Service) ([]folder, error) {
	resp, err := svc.Files.List().
		Context(ctx).
		Q("mimeType = 'application/vnd.google-apps.folder' and trashed = false and 'root' in parents").
		Fields("files(id,name)").
		PageSize(50).
		Do()
	if err != nil {
		return nil, err
	}
	out := make([]folder, 0, len(resp.Files))
	for _, f := range resp.Files {
		out = append(out, folder{ID: f.Id, Name: f.Name})
	}
	return out, nil
}

func chooseFolder(in *bufio.Reader, folders []folder) (string, error) {
	if len(folders) == 0 {
		infof("(no top-level folders found in your Drive)")
	} else {
		infof("Top-level folders in your Drive:")
		for i, f := range folders {
			infof("  %d. %s  (id: %s)", i+1, f.Name, f.ID)
		}
		infof("")
	}
	infof("Enter a number from the list, paste a folder id, or leave blank")
	infof("to use 'My Drive' root.")
	fmt.Fprint(os.Stderr, "Folder: ")
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	pick := strings.TrimSpace(line)
	if pick == "" {
		return "", nil
	}
	if n, err := strconv.Atoi(pick); err == nil {
		if n < 1 || n > len(folders) {
			return "", fmt.Errorf("number %d out of range (1..%d)", n, len(folders))
		}
		return folders[n-1].ID, nil
	}
	return pick, nil
}

func emitExports(clientID, clientSecret string, tok *oauth2.Token, folderID string) {
	// stdout — what the calling script `eval`s.
	emit := func(k, v string) {
		if v == "" {
			return
		}
		fmt.Printf("export %s=%s\n", k, shellQuote(v))
	}
	emit("HIVE_GDRIVE_CLIENT_ID", clientID)
	emit("HIVE_GDRIVE_CLIENT_SECRET", clientSecret)
	emit("HIVE_GDRIVE_ACCESS_TOKEN", tok.AccessToken)
	emit("HIVE_GDRIVE_REFRESH_TOKEN", tok.RefreshToken)
	emit("HIVE_GDRIVE_FOLDER_ID", folderID)
}

// shellQuote wraps v in single quotes, escaping any embedded ones.
// OAuth tokens contain only URL-safe characters, but client IDs can
// have dots and hyphens — quote everything for safety.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func randomState() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Random failure means the OS RNG is broken; the OAuth flow
		// can't proceed safely without a fresh state.
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// openBrowser is best-effort: if the platform's open command isn't
// available the user just pastes the URL manually.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	_ = cmd.Start()
}

func infof(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
