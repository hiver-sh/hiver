package firecracker

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/hugelgupf/p9/fsimpl/localfs"
	"github.com/hugelgupf/p9/p9"
)

// Workspace 9p ports. Each FUSE workspace is exported to the guest on its own
// vsock port starting at GuestFuseBasePort; the guest dials host CID 2 at
// that port and mounts it over 9p (trans=fd).
const GuestFuseBasePort uint32 = 1100

// HostVsockListener returns a unix-socket listener for guest→host connections
// on the given vsock port. Firecracker routes a guest connection to host
// CID 2 / port P to a unix socket at "<udsPath>_<P>", so the host listens
// there.
func HostVsockListener(udsPath string, port uint32) (net.Listener, error) {
	path := udsPath + "_" + strconv.FormatUint(uint64(port), 10)
	// Remove any stale socket from a previous run.
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen vsock host socket %s: %w", path, err)
	}
	return ln, nil
}

// Serve9P serves a 9P2000.L filesystem rooted at root over ln until ctx is
// cancelled or the listener closes. root is the host sbxfuse mountpoint, so
// every guest 9p operation lands on the host FUSE daemon — preserving its
// ACL enforcement, audit events, and remote-backend handling. New guest
// connections (one per mount(2)) are each served by the p9 server.
func Serve9P(ctx context.Context, root string, ln net.Listener) error {
	srv := p9.NewServer(localfs.Attacher(root))
	return srv.ServeContext(ctx, ln)
}

// MountFuseOption builds the mount(2) data string for a guest 9p mount over a
// connected vsock fd (trans=fd). The same fd is used for read and write.
func MountFuseOption(fd int) string {
	return fmt.Sprintf("trans=fd,rfdno=%d,wfdno=%d,version=9p2000.L,msize=262144,cache=none,access=client", fd, fd)
}
