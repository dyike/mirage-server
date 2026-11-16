package rfb

import (
	"errors"

	"github.com/dyike/mirage-server/internal/video"
)

func platformHEVC(cb func(video.Frame)) (h264Decoder, error) {
	return video.NewDecoder(video.HEVC, cb)
}

func hevcAccessUnits(data []byte) ([][][]byte, error) {
	nals, err := video.AppendAnnexBBounded(nil, data, 8192)
	if err != nil {
		return nil, err
	}
	if len(nals) == 0 {
		return nil, errors.New("rfb: HEVC rectangle is not Annex B")
	}
	var units [][][]byte
	start, picture := 0, false
	for i, nal := range nals {
		if len(nal) < 2 || nal[0]&128 != 0 || nal[1]&7 == 0 {
			return nil, errors.New("rfb: invalid HEVC NAL header")
		}
		kind := (nal[0] >> 1) & 63
		vcl := kind <= 31
		if vcl && len(nal) < 3 {
			return nil, errors.New("rfb: truncated HEVC slice")
		}
		boundary := kind == 32 || kind == 33 || kind == 34 || kind == 35 || kind == 39 || (vcl && nal[2]&128 != 0)
		if picture && boundary {
			units = append(units, nals[start:i])
			if len(units) >= 256 {
				return nil, errors.New("rfb: too many HEVC frames")
			}
			start, picture = i, false
		}
		picture = picture || vcl
	}
	if picture {
		units = append(units, nals[start:])
	}
	return units, nil
}
