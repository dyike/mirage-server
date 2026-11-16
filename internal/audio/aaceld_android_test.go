package audio

import (
	"testing"

	"github.com/ebitengine/purego"
)

func TestAndroidAACELDDecoderName(t *testing.T) {
	// Standalone adb tests need the Binder pool that ART starts in an APK.
	lib, err := purego.Dlopen("libbinder_ndk.so", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		t.Skip("run inside an APK on Android versions without the Binder NDK")
	}
	address, err := purego.Dlsym(lib, "ABinderProcess_startThreadPool")
	if err != nil {
		t.Fatal(err)
	}
	var start func()
	purego.RegisterFunc(&start, address)
	start()
	androidELD.Do(loadAndroidELD)
	if androidELD.err != nil {
		t.Fatal(androidELD.err)
	}
	if androidELD.name == nil || androidELD.releaseName == nil {
		t.Skip("component name queries require Android API 28")
	}
	p, err := NewAACELD(Format{SampleRate: 48000, Channels: 2, FramesPerPacket: 480, Cookie: AACELDCookie(RemoteDesktopELD)})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	name := p.Status().Decoder
	if name == "" || name == "MediaCodec" {
		t.Fatalf("actual component name was not reported: %q", name)
	}
	t.Logf("AAC-ELD decoder: %s", name)
}
