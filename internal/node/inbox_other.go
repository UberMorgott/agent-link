//go:build !windows

package node

import (
	"context"
	"io"
	"net"
)

// dialInbox connects to a Claude inbox's Unix socket.
func dialInbox(ctx context.Context, socket string) (io.WriteCloser, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", socket)
}
