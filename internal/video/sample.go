package video

import "slices"

// accessUnit reuses the Go storage needed to submit a length-prefixed
// sample to a platform decoder. Parameter sets configure the decoder and
// are not included in the sample. It is owned by one Decoder.
type accessUnit struct {
	sets [][]byte
	data []byte
}

func (a *accessUnit) prepare(codec Codec, nals [][]byte) {
	a.clearSets()
	size := 0
	for _, nal := range nals {
		if IsParameterSet(codec, nal) {
			a.sets = append(a.sets, nal)
		} else {
			size += 4 + len(nal)
		}
	}
	a.data = slices.Grow(a.data[:0], size)
	for _, nal := range nals {
		if !IsParameterSet(codec, nal) {
			a.data = append(a.data, byte(len(nal)>>24), byte(len(nal)>>16), byte(len(nal)>>8), byte(len(nal)))
			a.data = append(a.data, nal...)
		}
	}
}

// clearSets releases references to input packets while keeping scratch
// storage. The decoder copies any parameter sets it needs to retain.
func (a *accessUnit) clearSets() {
	clear(a.sets)
	a.sets = a.sets[:0]
}
