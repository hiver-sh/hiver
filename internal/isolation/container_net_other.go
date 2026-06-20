//go:build !linux

package isolation

import (
	"context"
	"errors"
)

func (c *container) setupPackedNet(context.Context, int, int) error {
	return errors.New("packed sandbox networking not supported on this platform")
}

func (c *container) teardownPackedNet(context.Context) {}
