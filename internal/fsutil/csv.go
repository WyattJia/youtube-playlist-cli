package fsutil

import (
	"encoding/csv"
	"strings"
	"unicode"
)

// WriteCSVRow prefixes spreadsheet formula triggers with an apostrophe.
func WriteCSVRow(w *csv.Writer, cells ...string) error {
	for i, cell := range cells {
		trimmed := strings.TrimLeftFunc(cell, func(r rune) bool {
			return unicode.IsSpace(r) && r != '\t' && r != '\r'
		})
		if trimmed == "" {
			continue
		}
		switch trimmed[0] {
		case '=', '+', '-', '@', '\t', '\r':
			cells[i] = "'" + cell
		}
	}
	return w.Write(cells)
}
