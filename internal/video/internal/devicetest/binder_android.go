//go:build android && cgo

// Package devicetest supplies the Binder runtime that ART normally starts for
// an APK. It is imported only by standalone adb tests, never the application.
package devicetest

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
static int start_binder(void) {
    void *lib = dlopen("libbinder_ndk.so", RTLD_NOW | RTLD_LOCAL);
    if (!lib) return 0;
    void (*start)(void) = dlsym(lib, "ABinderProcess_startThreadPool");
    void (*set_max)(uint32_t) = dlsym(lib, "ABinderProcess_setThreadPoolMaxThreadCount");
    if (!start || !set_max) return 0;
    set_max(2);
    start();
    return 1;
}
*/
import "C"

func StartBinderPool() bool { return C.start_binder() != 0 }
