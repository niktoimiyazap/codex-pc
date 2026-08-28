//go:build windows

package mcp

import (
	"encoding/base64"
	"fmt"
	"syscall"
	"unsafe"
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree          = kernel32.NewProc("LocalFree")
)

func unprotectSecret(ciphertext string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decode DPAPI ciphertext: %w", err)
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("empty DPAPI ciphertext")
	}
	in := dataBlob{cbData: uint32(len(raw)), pbData: &raw[0]}
	var out dataBlob
	ok, _, callErr := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&out)),
	)
	if ok == 0 {
		return "", fmt.Errorf("CryptUnprotectData failed: %v", callErr)
	}
	if out.pbData == nil || out.cbData == 0 {
		return "", fmt.Errorf("CryptUnprotectData returned no data")
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	plain := append([]byte(nil), unsafe.Slice(out.pbData, int(out.cbData))...)
	return string(plain), nil
}
