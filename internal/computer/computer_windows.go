//go:build windows

package computer

import (
	"fmt"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	getSystemMetrics = user32.NewProc("GetSystemMetrics")
	getCursorPos     = user32.NewProc("GetCursorPos")
	setCursorPos     = user32.NewProc("SetCursorPos")
	mouseEvent       = user32.NewProc("mouse_event")
	sendInput        = user32.NewProc("SendInput")
)

type point struct{ X, Y int32 }
type keybdInput struct {
	WVk, WScan  uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}
type input struct {
	Type uint32
	Pad  uint32
	Ki   keybdInput
	Tail [8]byte
}

func ScreenInfo() map[string]any {
	metric := func(i uintptr) int { r, _, _ := getSystemMetrics.Call(i); return int(int32(r)) }
	p := point{}
	_, _, _ = getCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return map[string]any{"x": metric(76), "y": metric(77), "width": metric(78), "height": metric(79), "cursor": map[string]any{"x": int(p.X), "y": int(p.Y)}}
}

func Move(x, y, durationMS int) (map[string]any, error) {
	if durationMS < 0 {
		durationMS = 0
	}
	if durationMS > 10000 {
		durationMS = 10000
	}
	p := point{}
	_, _, _ = getCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	if durationMS == 0 {
		r, _, e := setCursorPos.Call(uintptr(x), uintptr(y))
		if r == 0 {
			return nil, e
		}
	} else {
		steps := durationMS / 8
		if steps < 1 {
			steps = 1
		}
		if steps > 120 {
			steps = 120
		}
		for i := 1; i <= steps; i++ {
			ratio := float64(i) / float64(steps)
			nx := int(float64(p.X) + float64(x-int(p.X))*ratio)
			ny := int(float64(p.Y) + float64(y-int(p.Y))*ratio)
			r, _, e := setCursorPos.Call(uintptr(nx), uintptr(ny))
			if r == 0 {
				return nil, e
			}
			time.Sleep(time.Duration(durationMS/steps) * time.Millisecond)
		}
	}
	return map[string]any{"moved": true, "x": x, "y": y, "duration_ms": durationMS}, nil
}

func Click(x, y *int, button string, clicks int) (map[string]any, error) {
	if (x == nil) != (y == nil) {
		return nil, fmt.Errorf("x and y must be provided together")
	}
	if x != nil {
		if _, err := Move(*x, *y, 0); err != nil {
			return nil, err
		}
	}
	flags := map[string][2]uintptr{"left": {0x0002, 0x0004}, "right": {0x0008, 0x0010}, "middle": {0x0020, 0x0040}}
	pair, ok := flags[button]
	if !ok {
		return nil, fmt.Errorf("button must be left, right, or middle")
	}
	if clicks < 1 {
		clicks = 1
	}
	if clicks > 3 {
		clicks = 3
	}
	for i := 0; i < clicks; i++ {
		mouseEvent.Call(pair[0], 0, 0, 0, 0)
		mouseEvent.Call(pair[1], 0, 0, 0, 0)
		if i+1 < clicks {
			time.Sleep(80 * time.Millisecond)
		}
	}
	p := point{}
	_, _, _ = getCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return map[string]any{"clicked": true, "button": button, "clicks": clicks, "x": int(p.X), "y": int(p.Y)}, nil
}

func Scroll(dx, dy int) map[string]any {
	if dy != 0 {
		mouseEvent.Call(0x0800, 0, 0, uintptr(uint32(dy)), 0)
	}
	if dx != 0 {
		mouseEvent.Call(0x1000, 0, 0, uintptr(uint32(dx)), 0)
	}
	return map[string]any{"scrolled": true, "delta_x": dx, "delta_y": dy}
}

func sendKey(vk, scan uint16, flags uint32) error {
	in := input{Type: 1, Ki: keybdInput{WVk: vk, WScan: scan, DwFlags: flags}}
	r, _, e := sendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	if r != 1 {
		return e
	}
	return nil
}

func TypeText(text string, intervalMS int) (map[string]any, error) {
	if intervalMS < 0 {
		intervalMS = 0
	}
	if intervalMS > 1000 {
		intervalMS = 1000
	}
	units := utf16.Encode([]rune(text))
	for _, u := range units {
		if err := sendKey(0, u, 0x0004); err != nil {
			return nil, err
		}
		if err := sendKey(0, u, 0x0004|0x0002); err != nil {
			return nil, err
		}
		if intervalMS > 0 {
			time.Sleep(time.Duration(intervalMS) * time.Millisecond)
		}
	}
	return map[string]any{"typed": true, "characters": len([]rune(text)), "interval_ms": intervalMS}, nil
}

var vkMap = map[string]uint16{"BACKSPACE": 0x08, "TAB": 0x09, "ENTER": 0x0D, "SHIFT": 0x10, "CTRL": 0x11, "ALT": 0x12, "ESC": 0x1B, "SPACE": 0x20, "PAGEUP": 0x21, "PAGEDOWN": 0x22, "END": 0x23, "HOME": 0x24, "LEFT": 0x25, "UP": 0x26, "RIGHT": 0x27, "DOWN": 0x28, "DELETE": 0x2E, "WIN": 0x5B, "F1": 0x70, "F2": 0x71, "F3": 0x72, "F4": 0x73, "F5": 0x74, "F6": 0x75, "F7": 0x76, "F8": 0x77, "F9": 0x78, "F10": 0x79, "F11": 0x7A, "F12": 0x7B}

func Keypress(keys []string) (map[string]any, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("keys must not be empty")
	}
	resolved := make([]uint16, 0, len(keys))
	norm := make([]string, 0, len(keys))
	for _, raw := range keys {
		k := strings.ToUpper(strings.TrimSpace(raw))
		var vk uint16
		if len(k) == 1 && ((k[0] >= 'A' && k[0] <= 'Z') || (k[0] >= '0' && k[0] <= '9')) {
			vk = uint16(k[0])
		} else {
			var ok bool
			vk, ok = vkMap[k]
			if !ok {
				return nil, fmt.Errorf("unsupported key: %s", raw)
			}
		}
		resolved = append(resolved, vk)
		norm = append(norm, k)
	}
	for _, vk := range resolved {
		if err := sendKey(vk, 0, 0); err != nil {
			return nil, err
		}
	}
	for i := len(resolved) - 1; i >= 0; i-- {
		if err := sendKey(resolved[i], 0, 0x0002); err != nil {
			return nil, err
		}
	}
	return map[string]any{"pressed": norm}, nil
}
