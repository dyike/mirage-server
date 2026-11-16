package video

import "fmt"

// SPS dimensions are needed before MediaCodec can be configured. Strip the
// emulation-prevention bytes before reading the H.264/H.265 bit syntax.
type spsBits struct {
	data []byte
	pos  int
	err  bool
}

func (b *spsBits) read(n int) uint64 {
	if n < 0 || n > 64 || b.pos+n > len(b.data)*8 {
		b.err = true
		return 0
	}
	var v uint64
	for range n {
		v = v<<1 | uint64(b.data[b.pos/8]>>uint(7-b.pos%8)&1)
		b.pos++
	}
	return v
}

func (b *spsBits) ue() int {
	n := 0
	for !b.err && b.read(1) == 0 {
		n++
		if n > 30 {
			b.err = true
		}
	}
	return (1<<n - 1) + int(b.read(n))
}

func (b *spsBits) se() int {
	v := b.ue()
	if v&1 != 0 {
		return (v + 1) / 2
	}
	return -v / 2
}

func spsDimensions(codec Codec, nal []byte) (int, int, error) {
	header := 1
	if codec == HEVC {
		header = 2
	}
	if len(nal) <= header {
		return 0, 0, fmt.Errorf("video: truncated SPS")
	}
	b := spsBits{}
	zeros := 0
	for _, v := range nal[header:] {
		if zeros >= 2 && v == 3 {
			zeros = 0
			continue
		}
		b.data = append(b.data, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	var w, h int
	if codec == HEVC {
		b.read(4)
		layers := int(b.read(3))
		b.read(1)
		b.read(64) // general profile, compatibility and constraint flags
		b.read(32) // remaining constraints and level
		var profiles, levels [7]uint64
		for i := range layers {
			profiles[i], levels[i] = b.read(1), b.read(1)
		}
		if layers > 0 {
			b.read((8 - layers) * 2)
		}
		for i := range layers {
			if profiles[i] != 0 {
				b.read(64)
				b.read(24)
			}
			if levels[i] != 0 {
				b.read(8)
			}
		}
		b.ue() // SPS id
		chroma := b.ue()
		if chroma > 3 {
			b.err = true
		}
		if chroma == 3 && b.read(1) != 0 {
			chroma = 0 // separate_colour_plane_flag
		}
		w, h = b.ue(), b.ue()
		if b.read(1) != 0 {
			x, y := 1, 1
			if chroma == 1 || chroma == 2 {
				x = 2
			}
			if chroma == 1 {
				y = 2
			}
			left, right, top, bottom := b.ue(), b.ue(), b.ue(), b.ue()
			w -= (left + right) * x
			h -= (top + bottom) * y
		}
	} else {
		profile := b.read(8)
		b.read(16)
		b.ue()
		chroma := 1
		switch profile {
		case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
			chroma = b.ue()
			if chroma > 3 {
				b.err = true
			}
			if chroma == 3 && b.read(1) != 0 {
				chroma = 0
			}
			b.ue()
			b.ue()
			b.read(1)
			if b.read(1) != 0 {
				count := 8
				if chroma == 3 {
					count = 12
				}
				for i := range count {
					if b.read(1) == 0 {
						continue
					}
					size, last, next := 16, 8, 8
					if i >= 6 {
						size = 64
					}
					for range size {
						if next != 0 {
							next = (last + b.se() + 256) % 256
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
		b.ue()
		switch poc := b.ue(); poc {
		case 0:
			b.ue()
		case 1:
			b.read(1)
			b.se()
			b.se()
			n := b.ue()
			if n > 255 {
				b.err = true
			} else {
				for range n {
					b.se()
				}
			}
		case 2:
		default:
			b.err = true
		}
		b.ue()
		b.read(1)
		w, h = (b.ue()+1)*16, (b.ue()+1)*16
		frame := int(b.read(1))
		h *= 2 - frame
		if frame == 0 {
			b.read(1)
		}
		b.read(1)
		if b.read(1) != 0 {
			x, y := 1, 2-frame
			if chroma == 1 || chroma == 2 {
				x = 2
			}
			if chroma == 1 {
				y *= 2
			}
			left, right, top, bottom := b.ue(), b.ue(), b.ue(), b.ue()
			w -= (left + right) * x
			h -= (top + bottom) * y
		}
	}
	if b.err || w <= 0 || h <= 0 || w > 16384 || h > 16384 || int64(w)*int64(h) > 64<<20 {
		return 0, 0, fmt.Errorf("video: invalid %s SPS dimensions %dx%d", codec, w, h)
	}
	return w, h, nil
}
