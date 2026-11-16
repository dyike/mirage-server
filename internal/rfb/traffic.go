package rfb

// ReceivedBytes is the cumulative incoming TCP and UDP payload size, including
// RFB, video, audio and media control traffic, but not IP/TCP/UDP headers.
// It is safe to sample concurrently and never resets when media restarts or
// falls back to RFB pictures.
func (c *Client) ReceivedBytes() int64 {
	return c.received.Load() + c.mediaReceived.Load()
}
