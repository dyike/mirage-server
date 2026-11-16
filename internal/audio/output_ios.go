//go:build ios

package audio

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

var outputOnce sync.Once
var outputError error
var outputMu sync.Mutex
var outputUsers int

// All queues share the app's audio session. Keep it active until the last
// player closes, including when another connection is still playing.
func acquireOutput() (func(), error) {
	outputMu.Lock()
	defer outputMu.Unlock()
	err := withOutputSession(func(session objc.ID) error {
		sel := objc.RegisterName
		var nativeError objc.ID
		if outputUsers == 0 {
			category := objc.ID(objc.GetClass("NSString")).Send(sel("stringWithUTF8String:"), "AVAudioSessionCategoryPlayback")
			// MixWithOthers keeps local music playing and lets iOS choose the
			// current speaker, headphones or Bluetooth route.
			if !objc.Send[bool](session, sel("setCategory:withOptions:error:"), category, uintptr(1), &nativeError) {
				return outputSessionError("set playback category", nativeError)
			}
		}
		if !objc.Send[bool](session, sel("setActive:error:"), true, &nativeError) {
			return outputSessionError("activate playback", nativeError)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	outputUsers++
	return sync.OnceFunc(func() {
		outputMu.Lock()
		defer outputMu.Unlock()
		outputUsers--
		if outputUsers == 0 {
			_ = withOutputSession(func(session objc.ID) error {
				var nativeError objc.ID
				if !objc.Send[bool](session, objc.RegisterName("setActive:withOptions:error:"), false, uintptr(1), &nativeError) {
					return outputSessionError("deactivate playback", nativeError)
				}
				return nil
			})
		}
	}), nil
}

func resumeOutput() error {
	outputMu.Lock()
	defer outputMu.Unlock()
	return withOutputSession(func(session objc.ID) error {
		var nativeError objc.ID
		if !objc.Send[bool](session, objc.RegisterName("setActive:error:"), true, &nativeError) {
			return outputSessionError("resume playback", nativeError)
		}
		return nil
	})
}

func outputSessionError(action string, nativeError objc.ID) error {
	message := "unknown error"
	if nativeError != 0 {
		description := nativeError.Send(objc.RegisterName("localizedDescription"))
		message = objc.Send[string](description, objc.RegisterName("UTF8String"))
	}
	return fmt.Errorf("audio: AVAudioSession %s: %s", action, message)
}

func withOutputSession(fn func(objc.ID) error) error {
	outputOnce.Do(func() {
		_, outputError = purego.Dlopen("/System/Library/Frameworks/AVFAudio.framework/AVFAudio", purego.RTLD_NOW|purego.RTLD_LOCAL)
	})
	if outputError != nil {
		return fmt.Errorf("audio: AVFAudio: %w", outputError)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	sel := objc.RegisterName
	pool := objc.ID(objc.GetClass("NSAutoreleasePool")).Send(sel("new"))
	defer pool.Send(sel("drain"))
	class := objc.GetClass("AVAudioSession")
	if class == 0 {
		return fmt.Errorf("audio: AVAudioSession class unavailable")
	}
	session := objc.ID(class).Send(sel("sharedInstance"))
	if session == 0 {
		return fmt.Errorf("audio: AVAudioSession instance unavailable")
	}
	return fn(session)
}

// CurrentOutput only reads the session; diagnostics never activate playback.
func CurrentOutput() OutputStatus {
	var status OutputStatus
	err := withOutputSession(func(session objc.ID) error {
		sel := objc.RegisterName
		str := func(value objc.ID) string {
			if value == 0 {
				return ""
			}
			return objc.Send[string](value, sel("UTF8String"))
		}
		outputs := session.Send(sel("currentRoute")).Send(sel("outputs"))
		var ports []string
		for i, count := uintptr(0), uintptr(outputs.Send(sel("count"))); i < count; i++ {
			port := outputs.Send(sel("objectAtIndex:"), i)
			// Use port types, not personal device names or route UIDs.
			ports = append(ports, str(port.Send(sel("portType"))))
		}
		status = OutputStatus{Available: true, Category: str(session.Send(sel("category"))), Mode: str(session.Send(sel("mode"))),
			Route: strings.Join(ports, ", "), Volume: objc.Send[float32](session, sel("outputVolume")), VolumeKnown: true}
		return nil
	})
	if err != nil {
		status.Error = err.Error()
	}
	return status
}
