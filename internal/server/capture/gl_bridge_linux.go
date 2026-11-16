package capture

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/dyike/mirage-server/internal/server/input"
	"github.com/ebitengine/purego"
)

// Public GStreamer ABI prefixes, through GstGLBaseMemory.context.
type gstMiniObjectABI struct {
	Type                uintptr
	RefCount, LockState int32
	Flags               uint32
	Copy, Dispose, Free uintptr
	Private             uint32
	PrivatePointer      uintptr
}
type gstMemoryABI struct {
	Mini                                            gstMiniObjectABI
	Allocator, Parent, MaxSize, Align, Offset, Size uintptr
}
type gstGLMemoryABI struct {
	Memory  gstMemoryABI
	Context uintptr
}

var glBridge struct {
	once                               sync.Once
	err                                error
	threadAdd                          func(uintptr, uintptr, uintptr)
	textureID                          func(uintptr) uint32
	textureTarget                      func(uintptr) uint32
	isGLMemory                         func(uintptr) bool
	getProc                            func(string) uintptr
	display                            func() uintptr
	createImage                        func(uintptr, uintptr, uint32, uintptr, *int32) uintptr
	destroyImage                       func(uintptr, uintptr) bool
	imageTexture                       func(uint32, uintptr)
	genTextures, genFramebuffers       func(int32, *uint32)
	deleteTextures, deleteFramebuffers func(int32, *uint32)
	bindTexture, bindFramebuffer       func(uint32, uint32)
	framebufferTexture                 func(uint32, uint32, uint32, uint32, int32)
	checkFramebuffer                   func(uint32) uint32
	blit                               func(int32, int32, int32, int32, int32, int32, int32, int32, uint32, uint32)
	getInteger                         func(uint32, *int32)
	getError                           func() uint32
	finish                             func()
	isEnabled                          func(uint32) bool
	enable, disable                    func(uint32)
	callback                           uintptr
}
var glJobs sync.Map
var glJobID atomic.Uint64

type glBlitJob struct {
	memory uintptr
	target *input.DMABuffer
	err    error
}

func loadGLBridge() {
	for _, lib := range []struct {
		name    string
		symbols map[string]any
	}{
		{"libgstgl-1.0.so.0", map[string]any{"gst_gl_context_thread_add": &glBridge.threadAdd, "gst_gl_memory_get_texture_id": &glBridge.textureID, "gst_gl_memory_get_texture_target": &glBridge.textureTarget, "gst_is_gl_memory": &glBridge.isGLMemory}},
		{"libEGL.so.1", map[string]any{"eglGetProcAddress": &glBridge.getProc, "eglGetCurrentDisplay": &glBridge.display}},
	} {
		h, err := purego.Dlopen(lib.name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			glBridge.err = err
			return
		}
		for name, fn := range lib.symbols {
			p, err := purego.Dlsym(h, name)
			if err != nil {
				glBridge.err = err
				return
			}
			purego.RegisterFunc(fn, p)
		}
	}
	for name, fn := range map[string]any{
		"eglCreateImageKHR": &glBridge.createImage, "eglDestroyImageKHR": &glBridge.destroyImage,
		"glEGLImageTargetTexture2DOES": &glBridge.imageTexture,
		"glGenTextures":                &glBridge.genTextures, "glDeleteTextures": &glBridge.deleteTextures, "glBindTexture": &glBridge.bindTexture,
		"glGenFramebuffers": &glBridge.genFramebuffers, "glDeleteFramebuffers": &glBridge.deleteFramebuffers, "glBindFramebuffer": &glBridge.bindFramebuffer,
		"glFramebufferTexture2D": &glBridge.framebufferTexture, "glCheckFramebufferStatus": &glBridge.checkFramebuffer, "glBlitFramebuffer": &glBridge.blit,
		"glIsEnabled": &glBridge.isEnabled, "glEnable": &glBridge.enable, "glDisable": &glBridge.disable,
		"glGetIntegerv": &glBridge.getInteger, "glGetError": &glBridge.getError, "glFinish": &glBridge.finish,
	} {
		p := glBridge.getProc(name)
		if p == 0 {
			glBridge.err = fmt.Errorf("missing GPU bridge function %s", name)
			return
		}
		purego.RegisterFunc(fn, p)
	}
	glBridge.callback = purego.NewCallback(func(context, token uintptr) {
		if value, ok := glJobs.Load(token); ok {
			job := value.(*glBlitJob)
			job.err = job.blit()
		}
	})
}

// blit runs on the owning GL context thread. A GPU copy into a linear GBM
// target avoids gldownload's CPU readback and nonlinear export restriction.
func (j *glBlitJob) blit() error {
	const texture2D = 0x0de1
	const readFB = 0x8ca8
	const drawFB = 0x8ca9
	if glBridge.textureTarget(j.memory) != 1 {
		return fmt.Errorf("GPU composition requires a 2D texture")
	}
	display := glBridge.display()
	if display == 0 {
		return fmt.Errorf("GPU composition requires EGL")
	}
	b := j.target
	attrs := []int32{0x3057, int32(b.Width), 0x3056, int32(b.Height), 0x3271, int32(b.Format), 0x3272, int32(b.FD), 0x3273, 0, 0x3274, int32(b.Stride), 0x3038}
	image := glBridge.createImage(display, 0, 0x3270, 0, &attrs[0])
	if image == 0 {
		return fmt.Errorf("cannot import linear render target into EGL")
	}
	defer glBridge.destroyImage(display, image)
	var oldTex, oldRead, oldDraw int32
	glBridge.getInteger(0x8069, &oldTex)
	glBridge.getInteger(0x8caa, &oldRead)
	glBridge.getInteger(0x8ca6, &oldDraw)
	defer func() {
		glBridge.bindTexture(texture2D, uint32(oldTex))
		glBridge.bindFramebuffer(readFB, uint32(oldRead))
		glBridge.bindFramebuffer(drawFB, uint32(oldDraw))
	}()
	var tex uint32
	glBridge.genTextures(1, &tex)
	defer glBridge.deleteTextures(1, &tex)
	glBridge.bindTexture(texture2D, tex)
	glBridge.imageTexture(texture2D, image)
	var fb [2]uint32
	glBridge.genFramebuffers(2, &fb[0])
	defer glBridge.deleteFramebuffers(2, &fb[0])
	glBridge.bindFramebuffer(readFB, fb[0])
	glBridge.framebufferTexture(readFB, 0x8ce0, texture2D, glBridge.textureID(j.memory), 0)
	glBridge.bindFramebuffer(drawFB, fb[1])
	glBridge.framebufferTexture(drawFB, 0x8ce0, texture2D, tex, 0)
	if glBridge.checkFramebuffer(readFB) != 0x8cd5 || glBridge.checkFramebuffer(drawFB) != 0x8cd5 {
		return fmt.Errorf("incomplete GPU composition framebuffer")
	}
	if glBridge.isEnabled(0x0c11) {
		glBridge.disable(0x0c11)
		defer glBridge.enable(0x0c11)
	}
	glBridge.blit(0, 0, int32(b.Width), int32(b.Height), 0, 0, int32(b.Width), int32(b.Height), 0x4000, 0x2600)
	// Complete GPU writes before the target is imported by VA on another context.
	// This waits for completion; it does not map or transfer pixels to the CPU.
	glBridge.finish()
	if err := glBridge.getError(); err != 0 {
		return fmt.Errorf("GPU composition blit failed: %#x", err)
	}
	return nil
}
func blitGLBuffer(buffer uintptr, target *input.DMABuffer) error {
	glBridge.once.Do(loadGLBridge)
	if glBridge.err != nil {
		return glBridge.err
	}
	mem := dmaAPI.peekMemory(buffer, 0)
	if mem == 0 || !glBridge.isGLMemory(mem) {
		return fmt.Errorf("composition produced non-GPU memory")
	}
	ctx := (*gstGLMemoryABI)(unsafe.Pointer(mem)).Context
	if ctx == 0 {
		return fmt.Errorf("missing composition GL context")
	}
	job := &glBlitJob{memory: mem, target: target}
	token := uintptr(glJobID.Add(1))
	glJobs.Store(token, job)
	defer glJobs.Delete(token)
	glBridge.threadAdd(ctx, glBridge.callback, token)
	return job.err
}
