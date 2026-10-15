package textsafe

import "reflect"

// CleanAll applies Clean to every exported string reachable from ptr
// (struct fields, slice and array elements, pointers), in place. Map
// contents are left alone. Use it on data loaded from the Worker or git
// before a renderer that cannot filter its output (the TUI) draws it.
func CleanAll(ptr any) { walkStrings(ptr, Clean) }

// LineAll is CleanAll with Line: every string becomes one line. The TUIs
// use it because their tables and panes are built from fixed-width cells,
// and a goal or reason with a newline or a tab in it (the Worker allows
// both in multi-line fields) would otherwise draw a row of its own or push
// a pane's border off the screen.
func LineAll(ptr any) { walkStrings(ptr, Line) }

func walkStrings(ptr any, fn func(string) string) {
	v := reflect.ValueOf(ptr)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return
	}
	walkValue(v.Elem(), 0, fn)
}

func walkValue(v reflect.Value, depth int, fn func(string) string) {
	if depth > 32 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			if s := v.String(); s != "" {
				if t := fn(s); t != s {
					v.SetString(t)
				}
			}
		}
	case reflect.Pointer:
		// Values behind interfaces are not addressable and are skipped.
		if !v.IsNil() {
			walkValue(v.Elem(), depth+1, fn)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				walkValue(v.Field(i), depth+1, fn)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			walkValue(v.Index(i), depth+1, fn)
		}
	}
}
