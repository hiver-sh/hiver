//go:build !linux

package isolation

import (
	"context"
	"errors"
)

func (m *microvm) setupPackedNetMicrovm(context.Context, int, int, int) error {
	return errors.New("packed microvm networking not supported on this platform")
}

func (m *microvm) teardownPackedNetMicrovm(context.Context) {}
