package capture

import (
	"os"
	"sync/atomic"
	"testing"

	"github.com/dyike/mirage-server/internal/server/input"
	"golang.org/x/sys/unix"
)

func TestDMAOwnershipAndMapGuard(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_NATIVE_OUTPUT") == "" {
		t.Skip("requires GStreamer runtime")
	}
	a, err := newDMAAllocator()
	if err != nil {
		t.Fatal(err)
	}
	defer api.unref(a)
	fd, err := unix.MemfdCreate("mirage-dma-ownership-test", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err = unix.Ftruncate(fd, 4096); err != nil {
		t.Fatal(err)
	}
	var released atomic.Int32
	buffer, err := wrapDMA(a, &input.DMABuffer{FD: fd, Width: 16, Height: 16, Stride: 64, Format: 0x34325258}, func() { released.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	mem := dmaAPI.peekMemory(buffer, 0)
	dmaAPI.ref(mem)
	var info mapInfo
	before := dmaMapAttempts.Load()
	if dmaAPI.memoryMap(mem, &info, 1) {
		dmaAPI.memoryUnmap(mem, &info)
		t.Fatal("CPU mapping was permitted")
	}
	if dmaMapAttempts.Load() != before+1 {
		t.Fatal("CPU-map guard did not run")
	}
	api.miniUnref(buffer)
	if released.Load() != 0 {
		t.Fatal("capture buffer reused while downstream retains memory")
	}
	api.miniUnref(mem)
	if released.Load() != 1 {
		t.Fatal("buffer not released exactly once")
	}
	dmaReleases.Range(func(key, value any) bool { t.Error("DMA lifetime callback leaked"); return true })
}
