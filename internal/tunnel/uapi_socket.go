// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"fmt"
	"io"
	"net"
	"time"
)

// speak runs one command against the interface's UAPI socket.
//
// The protocol is one request per connection: write the command, close the
// write half so the daemon sees the end of it, read until EOF. Holding the
// connection open for a second command is the most common way to get a hang
// rather than an answer.
func speak(device, request string) (string, error) {
	path := socketPath(device)

	connection, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("%s is not answering on %s: %w", device, path, err)
	}
	defer connection.Close()

	// A tunnel that has wedged should report that rather than holding whoever
	// asked — the menu bar polls this every few seconds.
	if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "", err
	}

	if _, err := io.WriteString(connection, request); err != nil {
		return "", fmt.Errorf("writing to %s: %w", path, err)
	}
	if half, ok := connection.(*net.UnixConn); ok {
		if err := half.CloseWrite(); err != nil {
			return "", err
		}
	}

	response, err := io.ReadAll(connection)
	if err != nil {
		return "", fmt.Errorf("reading from %s: %w", path, err)
	}
	return string(response), nil
}
