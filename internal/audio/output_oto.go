//go:build windows || linux || android

package audio

func acquireOutput() (func(), error) { return nil, nil }
func resumeOutput() error            { return nil }

func CurrentOutput() OutputStatus {
	pcmContext.mu.Lock()
	context, ready, err := pcmContext.context, pcmContext.ready, pcmContext.err
	pcmContext.mu.Unlock()
	s := OutputStatus{Category: "System audio", Mode: "48 kHz stereo PCM", Route: "System default output"}
	if err != nil {
		s.Error = err.Error()
		return s
	}
	if context == nil {
		return s
	}
	if err := context.Err(); err != nil {
		s.Error = err.Error()
		return s
	}
	select {
	case <-ready:
		s.Available = true
	default:
	}
	return s
}
