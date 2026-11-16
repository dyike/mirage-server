package rfb

import (
	"sync"
	"testing"
)

func TestMediaDiagnosticsSnapshotAndPublication(t *testing.T) {
	var got MediaDiagnostics
	c := &Client{cfg: Config{OnMediaDiagnostics: func(d MediaDiagnostics) { got = d }}}
	c.updateMediaDiagnostics(func(d *MediaDiagnostics) {
		d.State = "Video active"
		d.Video.Codec, d.Video.HardwareKnown, d.Video.Hardware = "HEVC", true, false
		d.AudioPackets = 12
		d.AudioError = "AudioQueueStart: -50"
	})
	c.publishMediaDiagnostics(false)
	if got.Video.Hardware || !got.Video.HardwareKnown || got.AudioPackets != 12 || got.AudioError == "" {
		t.Fatalf("lost software/unknown distinction or playback error: %+v", got)
	}
	got.Video.Hardware = true
	if c.MediaDiagnostics().Video.Hardware {
		t.Fatal("consumer mutated live diagnostics")
	}
}

func TestMediaDiagnosticsConcurrentSnapshots(t *testing.T) {
	c := &Client{}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 100 {
				c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.AudioPackets++; d.AudioBytes = d.AudioPackets * 100 })
				d := c.MediaDiagnostics()
				if d.AudioBytes != d.AudioPackets*100 {
					t.Error("inconsistent snapshot")
				}
			}
		})
	}
	workers.Wait()
	if c.MediaDiagnostics().AudioPackets != 400 {
		t.Fatal("lost concurrent update")
	}
}
