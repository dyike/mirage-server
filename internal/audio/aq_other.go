//go:build !darwin && !windows && !linux && !android

package audio

// Player is unavailable here; NewAACELD and NewPCM always fail.
type Player struct{}

func NewAACELD(Format) (*Player, error) { return nil, ErrUnsupported }
func NewPCM(int, int) (*Player, error)  { return nil, ErrUnsupported }
func (*Player) Push([]byte) error       { return ErrUnsupported }
func (*Player) Dropped() int64          { return 0 }
func (*Player) Close()                  {}
func (*Player) Status() PlaybackStatus  { return PlaybackStatus{Error: ErrUnsupported.Error()} }
