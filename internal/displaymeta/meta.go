// Package displaymeta defines Mirage's opt-in ARD display metadata extension.
package displaymeta

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Encoding is a capability request, never a standalone framebuffer rectangle.
const Encoding int32 = -13055 // 0xffffcd01
const EncodingWire uint32 = 0xffffcd01

// Metadata follows the standard 1105 layout only when Encoding was requested.
// Older clients receive the unextended layout. Other servers ignore the request.
type Metadata struct {
	Names             map[uint32]string `json:"names"`
	SingleDisplayOnly bool              `json:"single_display_only"`
}

func Append(layout []byte, m Metadata) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(layout)+4+len(b) > 65535 {
		return nil, fmt.Errorf("display metadata exceeds ARD layout limit")
	}
	return append(append(layout, []byte("MRD1")...), b...), nil
}

func Parse(b []byte) (Metadata, error) {
	var m Metadata
	if !bytes.HasPrefix(b, []byte("MRD1")) {
		return m, nil
	}
	err := json.Unmarshal(b[4:], &m)
	return m, err
}
