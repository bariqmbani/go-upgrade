package main

import (
	"sort"
	"strconv"
	"strings"
)

// Style changes reference sanitized display bytes, so they take no columns and
// survive wrapping, resizing, and scrolling without retaining cursor controls.
type displayStyle struct {
	offset int64
	sgr    string
}

func (v *terminalView) setStyle(params string, offset int64) {
	if !v.color {
		return
	}
	if v.style == nil {
		v.style = make(map[int]string)
	}
	parts := strings.Split(params, ";")
	for i := 0; i < len(parts); i++ {
		code := 0
		if parts[i] != "" {
			var err error
			code, err = strconv.Atoi(parts[i])
			if err != nil {
				return
			}
		}
		switch {
		case code == 0:
			clear(v.style)
		case code == 1 || code == 2 || code == 3 || code == 4 || code == 5 || code == 7 || code == 8 || code == 9:
			v.style[code] = parts[i]
		case code == 22:
			delete(v.style, 1)
			delete(v.style, 2)
		case code == 23:
			delete(v.style, 3)
		case code == 24:
			delete(v.style, 4)
		case code == 25:
			delete(v.style, 5)
		case code == 27:
			delete(v.style, 7)
		case code == 28:
			delete(v.style, 8)
		case code == 29:
			delete(v.style, 9)
		case code >= 30 && code <= 37 || code >= 90 && code <= 97:
			v.style[38] = parts[i]
		case code >= 40 && code <= 47 || code >= 100 && code <= 107:
			v.style[48] = parts[i]
		case code == 39:
			delete(v.style, 38)
		case code == 49:
			delete(v.style, 48)
		case code == 38 || code == 48:
			count := 0
			if i+1 < len(parts) {
				switch parts[i+1] {
				case "5":
					count = 2
				case "2":
					count = 4
				}
			}
			if count == 0 || i+count >= len(parts) {
				return
			}
			for _, value := range parts[i+2 : i+count+1] {
				n, err := strconv.Atoi(value)
				if err != nil || n < 0 || n > 255 {
					return
				}
			}
			v.style[code] = strings.Join(parts[i:i+count+1], ";")
			i += count
		}
	}
	keys := make([]int, 0, len(v.style))
	for key := range v.style {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	codes := []string{"0"}
	for _, key := range keys {
		codes = append(codes, v.style[key])
	}
	sgr := "\x1b[" + strings.Join(codes, ";") + "m"
	if len(v.styles) > 0 && v.styles[len(v.styles)-1].offset == offset {
		v.styles[len(v.styles)-1].sgr = sgr
	} else {
		v.styles = append(v.styles, displayStyle{offset, sgr})
	}
}

func (v *terminalView) styledRow(row viewRow, text string) string {
	padded := paddedLine(text, v.width)
	if !v.color || len(v.styles) == 0 {
		return padded
	}
	index := sort.Search(len(v.styles), func(i int) bool { return v.styles[i].offset > row.start })
	var b strings.Builder
	b.WriteString("\x1b[0m")
	if index > 0 {
		b.WriteString(v.styles[index-1].sgr)
	}
	offset := row.start
	for index < len(v.styles) && v.styles[index].offset < row.end {
		change := v.styles[index]
		b.WriteString(text[offset-row.start : change.offset-row.start])
		b.WriteString(change.sgr)
		offset = change.offset
		index++
	}
	b.WriteString(text[offset-row.start:])
	b.WriteString("\x1b[0m")
	// Styling never extends to padding, the separator, or the progress footer.
	b.WriteString(padded[len(text):])
	return b.String()
}
