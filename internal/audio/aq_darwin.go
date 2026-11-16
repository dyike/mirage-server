//go:build darwin

package audio

import (
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// AudioQueue through purego, without cgo. The queue decodes AAC-ELD
// itself, given the stream format and the magic cookie; the client hands it
// one packet per buffer.

var aq struct {
	once sync.Once
	err  error

	newOutput    func(format unsafe.Pointer, cb, user, runLoop, mode uintptr, flags uint32, out *uintptr) int32
	setProperty  func(q uintptr, id uint32, data unsafe.Pointer, size uint32) int32
	getProperty  func(q uintptr, id uint32, data unsafe.Pointer, size *uint32) int32
	getParameter func(q uintptr, id uint32, value *float32) int32
	allocBuffer  func(q uintptr, size, descs uint32, out *uintptr) int32
	enqueue      func(q, buf uintptr, descs uint32, desc uintptr) int32
	start        func(q, at uintptr) int32
	stop         func(q uintptr, immediate bool) int32
	dispose      func(q uintptr, immediate bool) int32

	callback uintptr
}

func load() error {
	aq.once.Do(func() {
		h, err := purego.Dlopen("/System/Library/Frameworks/AudioToolbox.framework/AudioToolbox", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			aq.err = err
			return
		}
		purego.RegisterLibFunc(&aq.newOutput, h, "AudioQueueNewOutput")
		purego.RegisterLibFunc(&aq.setProperty, h, "AudioQueueSetProperty")
		purego.RegisterLibFunc(&aq.getProperty, h, "AudioQueueGetProperty")
		purego.RegisterLibFunc(&aq.getParameter, h, "AudioQueueGetParameter")
		purego.RegisterLibFunc(&aq.allocBuffer, h, "AudioQueueAllocateBufferWithPacketDescriptions")
		purego.RegisterLibFunc(&aq.enqueue, h, "AudioQueueEnqueueBuffer")
		purego.RegisterLibFunc(&aq.start, h, "AudioQueueStart")
		purego.RegisterLibFunc(&aq.stop, h, "AudioQueueStop")
		purego.RegisterLibFunc(&aq.dispose, h, "AudioQueueDispose")
		aq.callback = purego.NewCallback(bufferDone)
	})
	return aq.err
}

const (
	formatAACELD  = 0x61616365 // 'aace'
	formatLPCM    = 0x6c70636d // 'lpcm'
	pcmFlags      = 0x4 | 0x8  // kLinearPCMFormatFlagIsSignedInteger | IsPacked
	pcmBufferSize = 32 << 10   // ~170 ms of 48 kHz stereo; larger pushes are split
	propCookie    = 0x61716d63 // 'aqmc', kAudioQueueProperty_MagicCookie
	propRunning   = 0x6171726e // 'aqrn', kAudioQueueProperty_IsRunning
	bufferCount   = 16
	bufferSize    = 4096
	primePackets  = 3 // queued before playback starts: 30 ms at 480 frames
	idleTimeout   = 500 * time.Millisecond
	startGrace    = 100 * time.Millisecond // IsRunning can lag AudioQueueStart
	stallTimeout  = time.Second
	descSize      = 16
	offData       = 8  // AudioQueueBuffer.mAudioData
	offByteSize   = 16 // .mAudioDataByteSize
	offDescs      = 40 // .mPacketDescriptions
	offDescsCount = 48 // .mPacketDescriptionCount
)

// streamDescription is AudioStreamBasicDescription.
type streamDescription struct {
	SampleRate       float64
	FormatID         uint32
	FormatFlags      uint32
	BytesPerPacket   uint32
	FramesPerPacket  uint32
	BytesPerFrame    uint32
	ChannelsPerFrame uint32
	BitsPerChannel   uint32
	Reserved         uint32
}

var (
	playersMu sync.Mutex
	players   = map[uintptr]*Player{}
	nextID    uintptr
)

// Player plays one stream. Push and Close must not run concurrently.
type Player struct {
	id                      uintptr
	queue                   uintptr
	free                    chan uintptr // buffers back from the queue
	queued                  int
	primeBytes, queuedBytes int
	primeStep, primeLimit   int
	underruns               atomic.Int64
	started                 bool
	dropped                 atomic.Int64
	enqueued                atomic.Int64
	completed               atomic.Int64
	startRequested          atomic.Bool
	runningKnown            atomic.Bool
	running                 atomic.Bool
	volumeBits              atomic.Uint32
	volumeKnown             atomic.Bool
	lastError               atomic.Pointer[string]
	pcm                     bool // constant bit rate: no packet descriptions
	size                    int  // bytes per buffer
	releaseOutput           func()
	lastPacket              time.Time
	lastStart               time.Time
	progressAt              time.Time
	progressCount           int64
}

// NewAACELD returns a player for an AAC-ELD stream.
func NewAACELD(f Format) (*Player, error) {
	asbd := streamDescription{
		SampleRate: f.SampleRate, FormatID: formatAACELD,
		FramesPerPacket: uint32(f.FramesPerPacket), ChannelsPerFrame: uint32(f.Channels),
	}
	return newPlayer(asbd, f.Cookie, false, bufferSize)
}

// NewPCM returns a player for interleaved signed 16-bit little-endian PCM.
func NewPCM(sampleRate, channels int) (*Player, error) {
	if sampleRate <= 0 || channels < 1 || channels > 2 {
		return nil, fmt.Errorf("audio: unsupported PCM format %d Hz × %d", sampleRate, channels)
	}
	frame := uint32(2 * channels)
	asbd := streamDescription{
		SampleRate: float64(sampleRate), FormatID: formatLPCM, FormatFlags: pcmFlags,
		BytesPerPacket: frame, FramesPerPacket: 1, BytesPerFrame: frame,
		ChannelsPerFrame: uint32(channels), BitsPerChannel: 16,
	}
	return newPlayer(asbd, nil, true, pcmBufferSize)
}

func newPlayer(asbd streamDescription, cookie []byte, pcm bool, size int) (*Player, error) {
	if err := load(); err != nil {
		return nil, fmt.Errorf("audio: AudioToolbox: %w", err)
	}
	release, err := acquireOutput()
	if err != nil {
		return nil, err
	}
	p := &Player{free: make(chan uintptr, bufferCount), pcm: pcm, size: size, releaseOutput: release}
	playersMu.Lock()
	nextID++
	p.id = nextID
	players[p.id] = p
	playersMu.Unlock()
	if st := aq.newOutput(unsafe.Pointer(&asbd), aq.callback, p.id, 0, 0, 0, &p.queue); st != 0 {
		p.Close()
		return nil, fmt.Errorf("audio: AudioQueueNewOutput: %d", st)
	}
	if len(cookie) > 0 {
		if st := aq.setProperty(p.queue, propCookie, unsafe.Pointer(&cookie[0]), uint32(len(cookie))); st != 0 {
			p.Close()
			return nil, fmt.Errorf("audio: magic cookie rejected: %d", st)
		}
	}
	descs := uint32(1)
	if pcm {
		descs = 0
	}
	for range bufferCount {
		var b uintptr
		if st := aq.allocBuffer(p.queue, uint32(size), descs, &b); st != 0 {
			p.Close()
			return nil, fmt.Errorf("audio: AudioQueueAllocateBuffer: %d", st)
		}
		p.free <- b
	}
	return p, nil
}

func ptr(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) } // C memory

// Push queues one compressed packet. When every buffer is still queued the
// packet is dropped, which bounds the latency at 16 packets.
func (p *Player) Push(pkt []byte) error {
	if p.queue == 0 {
		return io.ErrClosedPipe
	}
	if len(pkt) == 0 || !p.pcm && len(pkt) > p.size {
		return nil
	}
	if err := p.prepare(time.Now()); err != nil {
		return p.recordError(err)
	}
	if p.pcm {
		// PCM has no packet boundaries: split it across buffers.
		for len(pkt) > 0 {
			n := min(len(pkt), p.size)
			if err := p.push(pkt[:n]); err != nil {
				return err
			}
			pkt = pkt[n:]
		}
		return nil
	}
	return p.push(pkt)
}

// A queue can stop independently of Push (iOS suspension/interruption), or
// remain running with no buffer callbacks after an underrun. Check before
// taking a free buffer so a full, stopped queue can also recover.
func (p *Player) prepare(now time.Time) error {
	idle := !p.lastPacket.IsZero() && now.Sub(p.lastPacket) >= idleTimeout
	p.lastPacket = now
	var volume float32
	if aq.getParameter(p.queue, 1, &volume) == 0 {
		p.volumeBits.Store(math.Float32bits(volume))
		p.volumeKnown.Store(true)
	}
	stopped, stalled := false, false
	if p.started {
		var running uint32
		size := uint32(4)
		if st := aq.getProperty(p.queue, propRunning, unsafe.Pointer(&running), &size); st != 0 {
			return fmt.Errorf("audio: AudioQueue running query: %d", st)
		}
		p.running.Store(running != 0)
		p.runningKnown.Store(true)
		stopped = running == 0 && now.Sub(p.lastStart) >= startGrace
		if completed := p.completed.Load(); completed != p.progressCount {
			p.progressCount, p.progressAt = completed, now
		}
		stalled = p.enqueued.Load() > p.completed.Load() && now.Sub(p.progressAt) >= stallTimeout
	}
	starved := p.pcm && p.primeBytes > 0 && p.started && p.enqueued.Load() == p.completed.Load()
	if starved {
		p.underruns.Add(1)
		p.primeBytes = min(p.primeLimit, p.primeBytes+p.primeStep)
	}
	if idle || stopped || stalled || starved {
		// Stop synchronously returns pending buffers and resets the AAC
		// decoder. Do not replay stale remote sound after returning to the app.
		if st := aq.stop(p.queue, true); st != 0 {
			return fmt.Errorf("audio: AudioQueueStop for recovery: %d", st)
		}
		p.started, p.queued, p.queuedBytes = false, 0, 0
		p.running.Store(false)
	}
	return nil
}

func (p *Player) startIfReady() error {
	if p.started || p.queued == 0 || !p.pcm && p.queued < primePackets || p.pcm && p.queuedBytes < p.primeBytes {
		return nil
	}
	// The app's audio session may have become inactive since the player
	// was created. Reactivate it on every start, not just on construction.
	if err := resumeOutput(); err != nil {
		return p.recordError(err)
	}
	p.startRequested.Store(true)
	if st := aq.start(p.queue, 0); st != 0 {
		return p.recordError(fmt.Errorf("audio: AudioQueueStart: %d", st))
	}
	p.started = true
	p.runningKnown.Store(false) // the previous stopped snapshot is now stale
	p.lastStart = time.Now()
	p.progressAt, p.progressCount = p.lastStart, p.completed.Load()
	p.lastError.Store(nil)
	return nil
}

func (p *Player) push(pkt []byte) error {
	// Retry a failed start even if all buffers are still queued.
	if err := p.startIfReady(); err != nil {
		return err
	}
	var b uintptr
	select {
	case b = <-p.free:
	default:
		p.dropped.Add(1)
		return nil
	}
	buf := ptr(b)
	copy(unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Add(buf, offData))), p.size), pkt)
	*(*uint32)(unsafe.Add(buf, offByteSize)) = uint32(len(pkt))
	if !p.pcm {
		desc := *(*unsafe.Pointer)(unsafe.Add(buf, offDescs))
		*(*int64)(desc) = 0                                 // mStartOffset
		*(*uint32)(unsafe.Add(desc, 8)) = 0                 // mVariableFramesInPacket
		*(*uint32)(unsafe.Add(desc, 12)) = uint32(len(pkt)) // mDataByteSize
		*(*uint32)(unsafe.Add(buf, offDescsCount)) = 1
	}
	if st := aq.enqueue(p.queue, b, 0, 0); st != 0 {
		p.free <- b
		return p.recordError(fmt.Errorf("audio: AudioQueueEnqueueBuffer: %d", st))
	}
	p.queued++
	if !p.started {
		p.queuedBytes += len(pkt)
	}
	p.enqueued.Add(1)
	return p.startIfReady()
}

func (p *Player) recordError(err error) error {
	message := err.Error()
	p.lastError.Store(&message)
	return err
}

// Status only reads atomics; it never touches a queue that may be closing.
func (p *Player) Status() PlaybackStatus {
	s := PlaybackStatus{Enqueued: p.enqueued.Load(), Completed: p.completed.Load(), Dropped: p.dropped.Load(),
		StartRequested: p.startRequested.Load(), RunningKnown: p.runningKnown.Load(), Running: p.running.Load(),
		Volume: math.Float32frombits(p.volumeBits.Load()), VolumeKnown: p.volumeKnown.Load(), Underruns: p.underruns.Load()}
	if !p.pcm {
		s.Decoder = "AudioToolbox"
	}
	if err := p.lastError.Load(); err != nil {
		s.Error = *err
	}
	return s
}

// Dropped counts packets dropped because the queue was full.
func (p *Player) Dropped() int64 { return p.dropped.Load() }

// Close stops playback and frees the queue.
func (p *Player) Close() {
	if p.queue != 0 {
		aq.stop(p.queue, true)
		aq.dispose(p.queue, true)
		p.queue = 0
	}
	p.running.Store(false)
	p.runningKnown.Store(true)
	p.unregister()
	if p.releaseOutput != nil {
		p.releaseOutput()
		p.releaseOutput = nil
	}
}

func (p *Player) unregister() {
	playersMu.Lock()
	delete(players, p.id)
	playersMu.Unlock()
}

// bufferDone is AudioQueueOutputCallback: the queue has played a buffer.
func bufferDone(user, _, buf uintptr) {
	playersMu.Lock()
	p := players[user]
	playersMu.Unlock()
	if p == nil {
		return
	}
	p.completed.Add(1)
	select {
	case p.free <- buf:
	default:
	}
}

func (p *Player) bufferNetworkPCM(rate, channels int) {
	p.primeBytes = rate * channels * 2 * 60 / 1000
	p.primeStep = rate * channels * 2 * 40 / 1000
	p.primeLimit = rate * channels * 2 * 200 / 1000
}
