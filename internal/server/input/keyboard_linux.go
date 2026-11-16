// Input behavior ported from WayVNC; Go implementation maintained by Mirage.
/*
 * Copyright (c) 2019 - 2025 Andri Yngvason
 *
 * Permission to use, copy, modify, and/or distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH
 * REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY
 * AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT,
 * INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM
 * LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE
 * OR OTHER TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR
 * PERFORMANCE OF THIS SOFTWARE.
 *
 * Acknowledgements: Reading Josef Gajdusek's wvnc code helped me understand
 * how to use the xkbcommon API to interface with the wayland virtual keyboard
 * interface.
 */

package input

import (
	"errors"
	"fmt"
	"log"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

type nativeInput interface {
	motion(float64, float64) error
	button(uint32, bool) error
	axis(uint32, float64, int32) error
	axisStop(uint32) error
	frame() error
	key(uint32, bool) error
	mods(uint32, uint32, uint32, uint32) error
	keymap([]byte) error
	close() error
}

var xkb struct {
	once           sync.Once
	err            error
	contextNew     func(uint32) uintptr
	contextFree    func(uintptr)
	mapNew         func(uintptr, *[5]*byte, uint32) uintptr
	mapFree        func(uintptr)
	stateNew       func(uintptr) uintptr
	stateFree      func(uintptr)
	mapString      func(uintptr, uint32) *byte
	free           func(unsafe.Pointer)
	minKey, maxKey func(uintptr) uint32
	layouts        func(uintptr) uint32
	levels         func(uintptr, uint32, uint32) uint32
	symbols        func(uintptr, uint32, uint32, uint32, **uint32) int32
	updateKey      func(uintptr, uint32, uint32) uint32
	updateMask     func(uintptr, uint32, uint32, uint32, uint32, uint32, uint32) uint32
	serialize      func(uintptr, uint32) uint32
	layout         func(uintptr, uint32) uint32
	level          func(uintptr, uint32, uint32) uint32
	levelMods      func(uintptr, uint32, uint32, uint32, *uint32, uintptr) uintptr
	led            func(uintptr, string) int32
}

func loadXKB() {
	h, e := purego.Dlopen("libxkbcommon.so.0", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if e != nil {
		xkb.err = e
		return
	}
	for n, f := range map[string]any{"xkb_context_new": &xkb.contextNew, "xkb_context_unref": &xkb.contextFree, "xkb_keymap_new_from_names": &xkb.mapNew, "xkb_keymap_unref": &xkb.mapFree, "xkb_state_new": &xkb.stateNew, "xkb_state_unref": &xkb.stateFree, "xkb_keymap_get_as_string": &xkb.mapString, "xkb_keymap_min_keycode": &xkb.minKey, "xkb_keymap_max_keycode": &xkb.maxKey, "xkb_keymap_num_layouts": &xkb.layouts, "xkb_keymap_num_levels_for_key": &xkb.levels, "xkb_keymap_key_get_syms_by_level": &xkb.symbols, "xkb_state_update_key": &xkb.updateKey, "xkb_state_update_mask": &xkb.updateMask, "xkb_state_serialize_mods": &xkb.serialize, "xkb_state_serialize_layout": &xkb.layout, "xkb_state_key_get_level": &xkb.level, "xkb_keymap_key_get_mods_for_level": &xkb.levelMods, "xkb_state_led_name_is_active": &xkb.led} {
		p, e := purego.Dlsym(h, n)
		if e != nil {
			xkb.err = e
			return
		}
		purego.RegisterFunc(f, p)
	}
	libc, e := purego.Dlopen("libc.so.6", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if e != nil {
		xkb.err = e
		return
	}
	p, e := purego.Dlsym(libc, "free")
	if e != nil {
		xkb.err = e
		return
	}
	purego.RegisterFunc(&xkb.free, p)
}

type keyEntry struct{ code, level, group uint32 }
type keyboard struct {
	pasteDiagnosticCode uint32
	ctx, keymap, state  uintptr
	native              nativeInput
	table               map[uint32][]keyEntry
	held                map[uint32]bool
	symbols             map[uint32]keyEntry
}

func newKeyboard(n nativeInput, layout, variant string) (*keyboard, error) {
	return newKeyboardWithOptions(n, Options{Layout: layout, Variant: variant})
}
func newKeyboardWithOptions(n nativeInput, o Options) (k *keyboard, err error) {
	xkb.once.Do(loadXKB)
	if xkb.err != nil {
		return nil, xkb.err
	}
	k = &keyboard{native: n, table: map[uint32][]keyEntry{}, held: map[uint32]bool{}, symbols: map[uint32]keyEntry{}}
	allocated := k
	defer func() {
		if err != nil {
			allocated.close()
		}
	}()
	k.ctx = xkb.contextNew(0)
	if k.ctx == 0 {
		return nil, errors.New("XKB context failed")
	}
	var names [5]*byte
	buffers := [5][]byte{}
	for i, value := range []string{o.Rules, o.Model, o.Layout, o.Variant, o.XKBOptions} {
		if strings.ContainsRune(value, 0) {
			return nil, errors.New("NUL in XKB configuration")
		}
		if value != "" {
			buffers[i] = append([]byte(value), 0)
			names[i] = &buffers[i][0]
		}
	}
	k.keymap = xkb.mapNew(k.ctx, &names, 0)
	runtime.KeepAlive(buffers)
	if k.keymap == 0 {
		return nil, errors.New("XKB keymap failed")
	}
	k.state = xkb.stateNew(k.keymap)
	if k.state == 0 {
		return nil, errors.New("XKB state failed")
	}
	minKey, maxKey := xkb.minKey(k.keymap), xkb.maxKey(k.keymap)
	if maxKey > 65535 {
		return nil, errors.New("XKB keymap too large")
	}
	for g := uint32(0); g < xkb.layouts(k.keymap); g++ {
		for code := minKey; code <= maxKey; code++ {
			for level := uint32(0); level < xkb.levels(k.keymap, code, g); level++ {
				var syms *uint32
				count := xkb.symbols(k.keymap, code, g, level, &syms)
				if count > 0 && count < 256 {
					for _, sym := range unsafe.Slice(syms, int(count)) {
						k.table[sym] = append(k.table[sym], keyEntry{code, level, g})
					}
				}
			}
		}
	}
	for sym := range k.table {
		sort.SliceStable(k.table[sym], func(i, j int) bool {
			a, b := k.table[sym][i], k.table[sym][j]
			if a.group != b.group {
				return a.group < b.group
			}
			return a.code < b.code
		})
	}
	text := xkb.mapString(k.keymap, 1)
	if text == nil {
		return nil, errors.New("XKB serialization failed")
	}
	defer xkb.free(unsafe.Pointer(text))
	var data []byte
	for i := uintptr(0); i < 4<<20; i++ {
		v := *(*byte)(unsafe.Add(unsafe.Pointer(text), i))
		data = append(data, v)
		if v == 0 {
			break
		}
	}
	if len(data) == 4<<20 {
		return nil, errors.New("XKB map too large")
	}
	if err = n.keymap(data); err != nil {
		return nil, err
	}
	return k, nil
}
func (k *keyboard) group() uint32 { return xkb.layout(k.state, 128) }
func (k *keyboard) masks() [3]uint32 {
	return [3]uint32{xkb.serialize(k.state, 1), xkb.serialize(k.state, 2), xkb.serialize(k.state, 4)}
}
func (k *keyboard) sendMods() error {
	m := k.masks()
	return k.native.mods(m[0], m[1], m[2], k.group())
}
func (k *keyboard) keycode(code uint32, down bool) error {
	if code < 8 || code > xkb.maxKey(k.keymap) {
		return fmt.Errorf("invalid XKB keycode %d", code)
	}
	if k.held[code] == down {
		return nil
	}
	if down {
		k.held[code] = true
	} else {
		delete(k.held, code)
	}
	old := k.group()
	changed := xkb.updateKey(k.state, code, boolWord(down))
	if changed&15 != 0 || old != k.group() {
		if err := k.sendMods(); err != nil {
			return err
		}
	}
	return k.sendKey(code-8, down)
}
func modifier(sym uint32) bool {
	return sym >= 0xffe1 && sym <= 0xffee || sym == 0xfe03 || sym == 0xfe11 || sym == 0xfe13 || sym == 0xff7f
}
func (k *keyboard) symbol(sym uint32, down bool) error {
	var chosen keyEntry
	found := false
	if !down {
		chosen, found = k.symbols[sym]
		if !found {
			return nil
		}
	} else {
		entries := k.table[sym]
		if len(entries) == 0 {
			return nil // Like WayVNC, ignore symbols absent from the configured layout.
		}
		group := k.group()
		groups := xkb.layouts(k.keymap)
		for offset := uint32(0); offset < groups && !found; offset++ {
			g := (group + offset) % groups
			for _, e := range entries {
				if e.group != g {
					continue
				}
				if !found {
					chosen, found = e, true
				}
				if modifier(sym) || xkb.level(k.state, e.code, g) == e.level {
					chosen = e
					break
				}
			}
		}
		if !found {
			return errors.New("no keyboard layout matches symbol")
		}
		if chosen.group != group {
			m := k.masks()
			xkb.updateMask(k.state, m[0], m[1], m[2], 0, 0, chosen.group)
		}
	}
	if k.held[chosen.code] == down {
		return nil
	}
	if down {
		k.symbols[sym] = chosen
		k.held[chosen.code] = true
	} else {
		delete(k.symbols, sym)
		delete(k.held, chosen.code)
	}
	matched := modifier(sym) || xkb.level(k.state, chosen.code, chosen.group) == chosen.level
	xkb.updateKey(k.state, chosen.code, boolWord(down))
	if err := k.sendMods(); err != nil {
		return err
	}
	if matched {
		return k.sendKey(chosen.code-8, down)
	}
	saved := k.masks()
	group := k.group()
	var mods uint32
	xkb.levelMods(k.keymap, chosen.code, chosen.group, chosen.level, &mods, 1)
	xkb.updateMask(k.state, mods, 0, 0, 0, 0, chosen.group)
	err := k.sendMods()
	if err == nil {
		err = k.sendKey(chosen.code-8, down)
	}
	xkb.updateMask(k.state, saved[0], saved[1], saved[2], 0, 0, group)
	restore := k.sendMods()
	if err != nil {
		return err
	}
	return restore
}
func (k *keyboard) reset() error {
	var first error
	for code := range k.held {
		if err := k.keycode(code, false); err != nil && first == nil {
			first = err
		}
	}
	clear(k.symbols)
	return first
}
func (k *keyboard) leds() uint32 {
	var bits uint32
	for i, name := range []string{"Scroll Lock", "Num Lock", "Caps Lock"} {
		if xkb.led(k.state, name) > 0 {
			bits |= 1 << i
		}
	}
	return bits
}
func (k *keyboard) close() {
	if k.state != 0 {
		xkb.stateFree(k.state)
	}
	if k.keymap != 0 {
		xkb.mapFree(k.keymap)
	}
	if k.ctx != 0 {
		xkb.contextFree(k.ctx)
	}
}

// Log paste triggers only, never ordinary typing or clipboard contents.
// Codes here are Linux evdev codes, as passed to the virtual keyboard.
func (k *keyboard) sendKey(code uint32, down bool) error {
	ctrl := k.held[29+8] || k.held[97+8]
	shift := k.held[42+8] || k.held[54+8]
	if down && ((code == 47 && ctrl) || (code == 110 && shift)) {
		k.pasteDiagnosticCode = code
		log.Printf("RFB paste input: keyboard=%p keycode=%d down=true ctrl=%t shift=%t", k, code, ctrl, shift)
	} else if !down && k.pasteDiagnosticCode != 0 && code == k.pasteDiagnosticCode {
		log.Printf("RFB paste input: keyboard=%p keycode=%d down=false", k, code)
		k.pasteDiagnosticCode = 0
	}
	return k.native.key(code, down)
}
