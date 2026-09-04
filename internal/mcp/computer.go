package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/niktoimiyazap/codexpc-connector/internal/computer"
)

func (s *Server) computer(ctx context.Context, args map[string]any) (map[string]any, error) {
	_ = ctx
	switch stringValue(args["action"]) {
	case "screen_info":
		return computer.ScreenInfo(), nil
	case "screenshot":
		pngData, info, err := computer.ScreenshotPNG()
		if err != nil {
			return nil, err
		}
		info["_image"] = base64.StdEncoding.EncodeToString(pngData)
		return info, nil
	case "move":
		return computer.Move(int(intValue(args["x"])), int(intValue(args["y"])), int(intValue(args["duration_ms"])))
	case "click":
		var x, y *int
		if _, ok := args["x"]; ok {
			xv := int(intValue(args["x"]))
			x = &xv
		}
		if _, ok := args["y"]; ok {
			yv := int(intValue(args["y"]))
			y = &yv
		}
		button := stringValue(args["button"])
		if button == "" {
			button = "left"
		}
		clicks := int(intValue(args["clicks"]))
		if clicks == 0 {
			clicks = 1
		}
		return computer.Click(x, y, button, clicks)
	case "scroll":
		return computer.Scroll(int(intValue(args["delta_x"])), int(intValue(args["delta_y"]))), nil
	case "type":
		return computer.TypeText(stringValue(args["text"]), int(intValue(args["interval_ms"])))
	case "keypress":
		var keys []string
		switch raw := args["keys"].(type) {
		case string:
			keys = strings.Fields(strings.ReplaceAll(raw, "+", " "))
		case []any:
			for _, item := range raw {
				keys = append(keys, stringValue(item))
			}
		}
		return computer.Keypress(keys)
	default:
		return nil, fmt.Errorf("unsupported computer action: %s", stringValue(args["action"]))
	}
}
