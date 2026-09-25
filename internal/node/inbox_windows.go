package node

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// dialInbox opens a Claude inbox's named pipe, waiting while every instance
// of it is busy (ERROR_PIPE_BUSY) until ctx ends.
func dialInbox(ctx context.Context, socket string) (io.WriteCloser, error) {
	for {
		f, err := os.OpenFile(socket, os.O_RDWR, 0) //nolint:gosec // G304: a Claude inbox pipe name (validInboxSocket), never a file
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
