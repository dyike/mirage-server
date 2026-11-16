package video

// GStreamer's H.264 DPB bump logic can retain a baseline picture when all
// reference slots are occupied, even though baseline has no frame reordering.
// Reserve one additional DPB slot in the decoder's copy of the SPS. This only
// changes the buffering upper bound; reference marking and slice data stay intact.
// Restrict the workaround to progressive baseline streams with an explicit VUI
// bitstream restriction and no reordering. Other profiles remain untouched.
func h264LowLatencySPS(nal []byte) []byte {
	if len(nal) < 5 || len(nal) > 4096 || nal[0]&31 != 7 || nal[1] != 66 {
		return nal
	}
	b := spsBits{}
	zeros := 0
	for _, v := range nal[1:] {
		if zeros == 2 && v == 3 {
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
	b.read(24)
	b.ue()
	b.ue()
	switch b.ue() {
	case 0:
		b.ue()
	case 1:
		b.read(1)
		b.se()
		b.se()
		n := b.ue()
		if n > 255 {
			return nal
		}
		for range n {
			b.se()
		}
	case 2:
	default:
		return nal
	}
	refs := b.ue()
	b.read(1)
	b.ue()
	b.ue()
	if b.read(1) == 0 {
		return nal
	}
	b.read(1)
	if b.read(1) != 0 {
		for range 4 {
			b.ue()
		}
	}
	if b.read(1) == 0 {
		return nal
	} // VUI
	if b.read(1) != 0 && b.read(8) == 255 {
		b.read(32)
	}
	if b.read(1) != 0 {
		b.read(1)
	}
	if b.read(1) != 0 {
		b.read(4)
		if b.read(1) != 0 {
			b.read(24)
		}
	}
	if b.read(1) != 0 {
		b.ue()
		b.ue()
	}
	if b.read(1) != 0 {
		b.read(32)
		b.read(32)
		b.read(1)
	}
	hrd := false
	for range 2 {
		if b.read(1) != 0 {
			hrd = true
			n := b.ue()
			if n > 31 {
				return nal
			}
			b.read(8)
			for range n + 1 {
				b.ue()
				b.ue()
				b.read(1)
			}
			b.read(20)
		}
	}
	if hrd {
		b.read(1)
	}
	b.read(1)
	if b.read(1) == 0 {
		return nal
	}
	b.read(1)
	for range 4 {
		b.ue()
	}
	reorder := b.ue()
	start := b.pos
	buffering := b.ue()
	end := b.pos
	if b.err || reorder != 0 || refs < 1 || refs >= 16 || buffering > refs {
		return nal
	}
	// Preserve the RBSP stop bit, then realign its padding after replacing ue(v).
	bits := make([]byte, 0, len(b.data)*8+8)
	stop := len(b.data)*8 - 1
	for stop >= 0 && b.data[stop/8]>>uint(7-stop%8)&1 == 0 {
		stop--
	}
	if stop < end {
		return nal
	}
	for i := 0; i < start; i++ {
		bits = append(bits, b.data[i/8]>>uint(7-i%8)&1)
	}
	value := refs + 2 // ue(refs+1)
	width := 0
	for v := value; v > 0; v >>= 1 {
		width++
	}
	for i := 0; i < width-1; i++ {
		bits = append(bits, 0)
	}
	for i := width - 1; i >= 0; i-- {
		bits = append(bits, byte(value>>i)&1)
	}
	for i := end; i <= stop; i++ {
		bits = append(bits, b.data[i/8]>>uint(7-i%8)&1)
	}
	rbsp := make([]byte, (len(bits)+7)/8)
	for i, v := range bits {
		rbsp[i/8] |= v << uint(7-i%8)
	}
	out := []byte{nal[0]}
	zeros = 0
	for _, v := range rbsp {
		if zeros == 2 && v <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}
