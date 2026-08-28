//go:build windows

package computer

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"syscall"
	"unsafe"
)

var (
	gdi32                   = syscall.NewLazyDLL("gdi32.dll")
	procCreateCompatibleDC  = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBmp = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject        = gdi32.NewProc("SelectObject")
	procBitBlt              = gdi32.NewProc("BitBlt")
	procGetDIBits           = gdi32.NewProc("GetDIBits")
	procDeleteObject        = gdi32.NewProc("DeleteObject")
	procDeleteDC            = gdi32.NewProc("DeleteDC")
	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")
)

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [3]uint32
}

func ScreenshotPNG() ([]byte, map[string]any, error) {
	info := ScreenInfo()
	x, _ := info["x"].(int)
	y, _ := info["y"].(int)
	width, _ := info["width"].(int)
	height, _ := info["height"].(int)
	if width <= 0 || height <= 0 {
		return nil, nil, fmt.Errorf("invalid virtual screen size")
	}

	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, nil, fmt.Errorf("GetDC failed")
	}
	defer procReleaseDC.Call(0, screenDC)

	memoryDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	if memoryDC == 0 {
		return nil, nil, fmt.Errorf("CreateCompatibleDC failed")
	}
	defer procDeleteDC.Call(memoryDC)

	bitmap, _, _ := procCreateCompatibleBmp.Call(screenDC, uintptr(width), uintptr(height))
	if bitmap == 0 {
		return nil, nil, fmt.Errorf("CreateCompatibleBitmap failed")
	}
	defer procDeleteObject.Call(bitmap)

	previous, _, _ := procSelectObject.Call(memoryDC, bitmap)
	if previous != 0 {
		defer procSelectObject.Call(memoryDC, previous)
	}

	const srccopy = 0x00CC0020
	const captureblt = 0x40000000
	ok, _, _ := procBitBlt.Call(memoryDC, 0, 0, uintptr(width), uintptr(height), screenDC, uintptr(x), uintptr(y), srccopy|captureblt)
	if ok == 0 {
		return nil, nil, fmt.Errorf("BitBlt failed")
	}

	stride := width * 4
	raw := make([]byte, stride*height)
	bi := bitmapInfo{}
	bi.Header = bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       int32(width),
		Height:      -int32(height),
		Planes:      1,
		BitCount:    32,
		Compression: 0,
		SizeImage:   uint32(len(raw)),
	}
	copied, _, _ := procGetDIBits.Call(memoryDC, bitmap, 0, uintptr(height), uintptr(unsafe.Pointer(&raw[0])), uintptr(unsafe.Pointer(&bi)), 0)
	if int(copied) != height {
		return nil, nil, fmt.Errorf("GetDIBits copied %d/%d rows", copied, height)
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for yy := 0; yy < height; yy++ {
		row := yy * stride
		for xx := 0; xx < width; xx++ {
			i := row + xx*4
			img.SetRGBA(xx, yy, color.RGBA{R: raw[i+2], G: raw[i+1], B: raw[i], A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, nil, err
	}
	return out.Bytes(), info, nil
}
