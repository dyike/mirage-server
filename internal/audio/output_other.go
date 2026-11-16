//go:build !ios && !windows && !linux && !android

package audio

func acquireOutput() (func(), error) { return nil, nil }

func resumeOutput() error { return nil }

func CurrentOutput() OutputStatus {
	return OutputStatus{}
}
