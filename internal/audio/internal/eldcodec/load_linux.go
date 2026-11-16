//go:build linux && !android

package eldcodec

import "github.com/ebitengine/purego"

func loadLibrary(path string) (func(string) (uintptr, error), error) {
	h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, err
	}
	return func(name string) (uintptr, error) { return purego.Dlsym(h, name) }, nil
}
