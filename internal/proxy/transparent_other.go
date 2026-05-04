//go:build !linux

package proxy

import (
	"context"
	"errors"
)

// ServeTransparent is unavailable off Linux: it depends on iptables
// REDIRECT and SO_ORIGINAL_DST.
func (p *Proxy) ServeTransparent(_ context.Context, _ string) error {
	return errors.New("proxy: ServeTransparent only supported on Linux")
}
