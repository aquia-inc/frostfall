package browser

import "github.com/go-rod/rod/lib/input"

// keyFor maps step-vocabulary key names to rod input keys. Single characters
// pass through; unknown names fall back to their first rune.
func keyFor(name string) input.Key {
	switch name {
	case "Tab":
		return input.Tab
	case "Enter":
		return input.Enter
	case "Escape", "Esc":
		return input.Escape
	case "Space":
		return input.Space
	case "ArrowUp", "Up":
		return input.ArrowUp
	case "ArrowDown", "Down":
		return input.ArrowDown
	case "ArrowLeft", "Left":
		return input.ArrowLeft
	case "ArrowRight", "Right":
		return input.ArrowRight
	case "Home":
		return input.Home
	case "End":
		return input.End
	case "PageUp":
		return input.PageUp
	case "PageDown":
		return input.PageDown
	case "Backspace":
		return input.Backspace
	case "Delete":
		return input.Delete
	}
	return input.Key([]rune(name)[0])
}
