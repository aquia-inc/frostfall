package browser

import (
	"fmt"

	"github.com/go-rod/rod/lib/input"
)

var namedKeys = map[string]input.Key{
	"Tab":        input.Tab,
	"Enter":      input.Enter,
	"Escape":     input.Escape,
	"Esc":        input.Escape,
	"Space":      input.Space,
	"ArrowUp":    input.ArrowUp,
	"Up":         input.ArrowUp,
	"ArrowDown":  input.ArrowDown,
	"Down":       input.ArrowDown,
	"ArrowLeft":  input.ArrowLeft,
	"Left":       input.ArrowLeft,
	"ArrowRight": input.ArrowRight,
	"Right":      input.ArrowRight,
	"Home":       input.Home,
	"End":        input.End,
	"PageUp":     input.PageUp,
	"PageDown":   input.PageDown,
	"Backspace":  input.Backspace,
	"Delete":     input.Delete,
}

// keyFor maps step-vocabulary key names to rod input keys. Single characters
// pass through; an unrecognized multi-character name is an error — silently
// pressing its first rune ("Ctrl+A" pressing C) would let a typo'd test keep
// passing while exercising nothing.
func keyFor(name string) (input.Key, error) {
	if k, ok := namedKeys[name]; ok {
		return k, nil
	}
	runes := []rune(name)
	if len(runes) == 1 {
		return input.Key(runes[0]), nil
	}
	return 0, fmt.Errorf("unknown key name %q (single characters or: Tab, Enter, Escape, Space, arrows, Home, End, PageUp, PageDown, Backspace, Delete)", name)
}
