from __future__ import annotations

import ctypes
import os
import struct
import time
import zlib
from ctypes import wintypes
from typing import Any


class ComputerUseError(RuntimeError):
    pass


def _require_windows() -> None:
    if os.name != "nt":
        raise ComputerUseError("computer tool currently supports Windows only")


def _user32() -> Any:
    _require_windows()
    return ctypes.WinDLL("user32", use_last_error=True)


def _gdi32() -> Any:
    _require_windows()
    return ctypes.WinDLL("gdi32", use_last_error=True)


def screen_info() -> dict[str, int]:
    user32 = _user32()
    return {
        "x": int(user32.GetSystemMetrics(76)),  # SM_XVIRTUALSCREEN
        "y": int(user32.GetSystemMetrics(77)),  # SM_YVIRTUALSCREEN
        "width": int(user32.GetSystemMetrics(78)),  # SM_CXVIRTUALSCREEN
        "height": int(user32.GetSystemMetrics(79)),  # SM_CYVIRTUALSCREEN
    }


def cursor_position() -> dict[str, int]:
    user32 = _user32()
    point = wintypes.POINT()
    if not user32.GetCursorPos(ctypes.byref(point)):
        raise ctypes.WinError(ctypes.get_last_error())
    return {"x": int(point.x), "y": int(point.y)}


def move(x: int, y: int, duration_ms: int = 0) -> dict[str, Any]:
    user32 = _user32()
    start = cursor_position()
    duration_ms = max(0, min(int(duration_ms), 10_000))
    if duration_ms == 0:
        if not user32.SetCursorPos(int(x), int(y)):
            raise ctypes.WinError(ctypes.get_last_error())
    else:
        steps = max(1, min(120, duration_ms // 8))
        for index in range(1, steps + 1):
            ratio = index / steps
            next_x = round(start["x"] + (int(x) - start["x"]) * ratio)
            next_y = round(start["y"] + (int(y) - start["y"]) * ratio)
            if not user32.SetCursorPos(next_x, next_y):
                raise ctypes.WinError(ctypes.get_last_error())
            time.sleep(duration_ms / steps / 1000)
    return {"moved": True, "x": int(x), "y": int(y), "duration_ms": duration_ms}


def _mouse_event(flags: int, data: int = 0) -> None:
    user32 = _user32()
    user32.mouse_event(flags, 0, 0, data, 0)


def click(x: int | None = None, y: int | None = None, button: str = "left", clicks: int = 1) -> dict[str, Any]:
    if x is not None or y is not None:
        if x is None or y is None:
            raise ValueError("x and y must be provided together")
        move(int(x), int(y))
    button_flags = {
        "left": (0x0002, 0x0004),
        "right": (0x0008, 0x0010),
        "middle": (0x0020, 0x0040),
    }
    if button not in button_flags:
        raise ValueError("button must be left, right, or middle")
    clicks = max(1, min(int(clicks), 3))
    down, up = button_flags[button]
    for index in range(clicks):
        _mouse_event(down)
        _mouse_event(up)
        if index + 1 < clicks:
            time.sleep(0.08)
    position = cursor_position()
    return {"clicked": True, "button": button, "clicks": clicks, **position}


def scroll(delta_y: int, delta_x: int = 0) -> dict[str, int | bool]:
    if delta_y:
        _mouse_event(0x0800, int(delta_y))  # MOUSEEVENTF_WHEEL
    if delta_x:
        _mouse_event(0x01000, int(delta_x))  # MOUSEEVENTF_HWHEEL
    return {"scrolled": True, "delta_x": int(delta_x), "delta_y": int(delta_y)}


class _KEYBDINPUT(ctypes.Structure):
    _fields_ = [
        ("wVk", wintypes.WORD),
        ("wScan", wintypes.WORD),
        ("dwFlags", wintypes.DWORD),
        ("time", wintypes.DWORD),
        ("dwExtraInfo", ctypes.POINTER(ctypes.c_ulong)),
    ]


class _INPUT_UNION(ctypes.Union):
    _fields_ = [("ki", _KEYBDINPUT)]


class _INPUT(ctypes.Structure):
    _anonymous_ = ("union",)
    _fields_ = [("type", wintypes.DWORD), ("union", _INPUT_UNION)]


def _send_key(vk: int, key_up: bool = False) -> None:
    user32 = _user32()
    entry = _INPUT(type=1, ki=_KEYBDINPUT(vk, 0, 0x0002 if key_up else 0, 0, None))
    if user32.SendInput(1, ctypes.byref(entry), ctypes.sizeof(_INPUT)) != 1:
        raise ctypes.WinError(ctypes.get_last_error())


def _send_unicode(character: str, key_up: bool = False) -> None:
    user32 = _user32()
    entry = _INPUT(type=1, ki=_KEYBDINPUT(0, ord(character), 0x0004 | (0x0002 if key_up else 0), 0, None))
    if user32.SendInput(1, ctypes.byref(entry), ctypes.sizeof(_INPUT)) != 1:
        raise ctypes.WinError(ctypes.get_last_error())


def type_text(text: str, interval_ms: int = 0) -> dict[str, int | bool]:
    interval_ms = max(0, min(int(interval_ms), 1000))
    for character in str(text):
        encoded = character.encode("utf-16-le")
        for index in range(0, len(encoded), 2):
            code_unit = chr(int.from_bytes(encoded[index : index + 2], "little"))
            _send_unicode(code_unit)
            _send_unicode(code_unit, key_up=True)
        if interval_ms:
            time.sleep(interval_ms / 1000)
    return {"typed": True, "characters": len(str(text)), "interval_ms": interval_ms}


_VK = {
    "BACKSPACE": 0x08,
    "TAB": 0x09,
    "ENTER": 0x0D,
    "SHIFT": 0x10,
    "CTRL": 0x11,
    "ALT": 0x12,
    "ESC": 0x1B,
    "SPACE": 0x20,
    "PAGEUP": 0x21,
    "PAGEDOWN": 0x22,
    "END": 0x23,
    "HOME": 0x24,
    "LEFT": 0x25,
    "UP": 0x26,
    "RIGHT": 0x27,
    "DOWN": 0x28,
    "DELETE": 0x2E,
    "WIN": 0x5B,
}
_VK.update({f"F{index}": 0x6F + index for index in range(1, 13)})


def keypress(keys: list[str]) -> dict[str, Any]:
    if not keys:
        raise ValueError("keys must not be empty")
    resolved: list[int] = []
    normalized: list[str] = []
    for raw in keys:
        key = str(raw).strip().upper()
        vk = ord(key) if len(key) == 1 and key.isascii() and key.isalnum() else _VK.get(key)
        if vk is None:
            raise ValueError(f"Unsupported key: {raw}")
        resolved.append(vk)
        normalized.append(key)
    for vk in resolved:
        _send_key(vk)
    for vk in reversed(resolved):
        _send_key(vk, key_up=True)
    return {"pressed": normalized}


def _png_chunk(kind: bytes, data: bytes) -> bytes:
    return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)


def screenshot_png() -> tuple[bytes, dict[str, int]]:
    user32 = _user32()
    gdi32 = _gdi32()
    info = screen_info()
    x, y, width, height = info["x"], info["y"], info["width"], info["height"]
    if width <= 0 or height <= 0:
        raise ComputerUseError("Windows returned an invalid virtual screen size")

    screen_dc = user32.GetDC(0)
    memory_dc = gdi32.CreateCompatibleDC(screen_dc)
    bitmap = gdi32.CreateCompatibleBitmap(screen_dc, width, height)
    previous = gdi32.SelectObject(memory_dc, bitmap)
    try:
        if not gdi32.BitBlt(memory_dc, 0, 0, width, height, screen_dc, x, y, 0x00CC0020 | 0x40000000):
            raise ctypes.WinError(ctypes.get_last_error())

        class BITMAPINFOHEADER(ctypes.Structure):
            _fields_ = [
                ("biSize", wintypes.DWORD),
                ("biWidth", wintypes.LONG),
                ("biHeight", wintypes.LONG),
                ("biPlanes", wintypes.WORD),
                ("biBitCount", wintypes.WORD),
                ("biCompression", wintypes.DWORD),
                ("biSizeImage", wintypes.DWORD),
                ("biXPelsPerMeter", wintypes.LONG),
                ("biYPelsPerMeter", wintypes.LONG),
                ("biClrUsed", wintypes.DWORD),
                ("biClrImportant", wintypes.DWORD),
            ]

        class BITMAPINFO(ctypes.Structure):
            _fields_ = [("bmiHeader", BITMAPINFOHEADER), ("bmiColors", wintypes.DWORD * 3)]

        bitmap_info = BITMAPINFO()
        bitmap_info.bmiHeader = BITMAPINFOHEADER(
            ctypes.sizeof(BITMAPINFOHEADER), width, -height, 1, 32, 0, width * height * 4, 0, 0, 0, 0
        )
        buffer = ctypes.create_string_buffer(width * height * 4)
        copied = gdi32.GetDIBits(memory_dc, bitmap, 0, height, buffer, ctypes.byref(bitmap_info), 0)
        if copied != height:
            raise ctypes.WinError(ctypes.get_last_error())
        raw = memoryview(buffer.raw)
        rows = bytearray()
        stride = width * 4
        for row_index in range(height):
            row = raw[row_index * stride : (row_index + 1) * stride]
            rows.append(0)
            for pixel_index in range(0, stride, 4):
                blue, green, red = row[pixel_index], row[pixel_index + 1], row[pixel_index + 2]
                rows.extend((red, green, blue))
        header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
        png = (
            b"\x89PNG\r\n\x1a\n"
            + _png_chunk(b"IHDR", header)
            + _png_chunk(b"IDAT", zlib.compress(rows, 6))
            + _png_chunk(b"IEND", b"")
        )
        return png, info
    finally:
        if previous:
            gdi32.SelectObject(memory_dc, previous)
        if bitmap:
            gdi32.DeleteObject(bitmap)
        if memory_dc:
            gdi32.DeleteDC(memory_dc)
        if screen_dc:
            user32.ReleaseDC(0, screen_dc)


def perform(action: str, arguments: dict[str, Any]) -> dict[str, Any]:
    action = action.strip().lower()
    if action == "screen_info":
        return {**screen_info(), "cursor": cursor_position()}
    if action == "screenshot":
        image, info = screenshot_png()
        return {"_mcp_image": {"data": image, "mime_type": "image/png"}, **info}
    if action == "move":
        return move(int(arguments["x"]), int(arguments["y"]), int(arguments.get("duration_ms", 0)))
    if action == "click":
        return click(
            arguments.get("x"),
            arguments.get("y"),
            str(arguments.get("button", "left")),
            int(arguments.get("clicks", 1)),
        )
    if action == "scroll":
        return scroll(int(arguments.get("delta_y", 0)), int(arguments.get("delta_x", 0)))
    if action == "type":
        return type_text(str(arguments.get("text", "")), int(arguments.get("interval_ms", 0)))
    if action == "keypress":
        raw_keys = arguments.get("keys")
        if isinstance(raw_keys, str):
            raw_keys = [part for part in raw_keys.replace("+", " ").split() if part]
        return keypress([str(value) for value in (raw_keys or [])])
    raise ValueError(f"Unsupported computer action: {action}")
