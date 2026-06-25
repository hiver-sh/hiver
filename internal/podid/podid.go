// Package podid encodes a pod's IPv4 address as the sandbox routing id.
//
// The id doubles as the route target: it is a UUID whose leading four bytes are
// the host pod's IPv4 address (the rest stay zero). The gateway decodes these
// bytes back to the pod IP and dials it directly, so no per-sandbox Service or
// DNS lookup is needed. Both the controller (when listing) and sandboxd (when
// answering a create) must produce the same encoding, so it lives here.
package podid

import (
	"fmt"
	"net"

	"github.com/google/uuid"
)

// FromIP encodes an IPv4 address into a UUID by packing its four octets into the
// leading bytes. See the package doc for why the id and the route target are the
// same value.
func FromIP(ip string) (uuid.UUID, error) {
	v4 := net.ParseIP(ip).To4()
	if v4 == nil {
		return uuid.Nil, fmt.Errorf("pod IP %q is not IPv4", ip)
	}
	var u uuid.UUID
	copy(u[:4], v4)
	return u, nil
}
