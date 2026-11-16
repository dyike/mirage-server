package audio

// NewNetworkPCM adds a short preroll on push-based native queues. TCP video
// bursts can delay small audio packets; reprime after draining instead of
// repeatedly starting playback with only one 20 ms packet. Pull-based backends
// already have their own device buffer and silence-preserving stream reader.
func NewNetworkPCM(rate, channels int) (*Player, error) {
	p, err := NewPCM(rate, channels)
	if err != nil {
		return nil, err
	}
	if buffered, ok := any(p).(interface{ bufferNetworkPCM(int, int) }); ok {
		buffered.bufferNetworkPCM(rate, channels)
	}
	return p, nil
}
