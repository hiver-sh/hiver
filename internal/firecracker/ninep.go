package firecracker

import (
	"context"
	"fmt"
	"net"

	"github.com/hugelgupf/p9/fsimpl/localfs"
	"github.com/hugelgupf/p9/p9"
)

// Workspace 9p ports. Each FUSE workspace is exported to the guest on its own
// TCP port starting at GuestFuseBasePort; the guest dials the netns gateway
// (its eth0 gateway) at that port and mounts it over 9p (trans=fd). The host
// runs the per-mount listener inside the VM's netns (see listenTCPInNetns), so
// the connection survives a snapshot resume (unlike the former vsock socket).
const GuestFuseBasePort uint32 = 1100

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
