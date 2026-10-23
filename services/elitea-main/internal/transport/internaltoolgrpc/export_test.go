package internaltoolgrpc

import "time"

// SetKeepaliveInterval shortens the keepalive for a test and restores it.
func SetKeepaliveInterval(interval time.Duration) (restore func()) {
	previous := keepaliveInterval
	keepaliveInterval = interval
	return func() { keepaliveInterval = previous }
}
