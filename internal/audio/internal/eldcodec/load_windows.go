package eldcodec

import "golang.org/x/sys/windows"

func loadLibrary(path string) (func(string) (uintptr, error), error) {
	h, err := windows.LoadLibraryEx(path, 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return nil, err
	}
	return func(name string) (uintptr, error) { return windows.GetProcAddress(h, name) }, nil
}
