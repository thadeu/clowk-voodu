package table

import (
	"fmt"
	"strings"
)

// Render renders the table based on the Type
func (t *Table) Render() string {
	switch t.Type {
	case "ascii":
		return t.Ascii()
	case "text":
		return t.Text()
	case "table":
		return t.Table()
	default:
		return t.Table()
	}
}

// Table renders the table in Heroku-style format (with dashes)
func (t *Table) Table() string {
	if len(t.headers) == 0 {
		return ""
	}

	widths := t.calculateWidths()
	var result strings.Builder

	// Build format string
	formatParts := make([]string, len(t.headers))
	for i := range t.headers {
		if i == len(t.headers)-1 {
			formatParts[i] = "%s"
		} else {
			formatParts[i] = fmt.Sprintf("%%-%ds", widths[i])
		}
	}
	formatStr := strings.Join(formatParts, " ") + "\n"

	// Print header
	args := make([]interface{}, len(t.headers))
	for i, header := range t.headers {
		if i == len(t.headers)-1 {
			args[i] = colorGreenHeader(header)
		} else {
			args[i] = formatHeaderWithColor(header, widths[i])
		}
	}
	result.WriteString(fmt.Sprintf(formatStr, args...))

	// Print separator
	separatorArgs := make([]interface{}, len(t.headers))
	for i := range t.headers {
		separatorArgs[i] = strings.Repeat("-", widths[i])
	}
	result.WriteString(fmt.Sprintf(formatStr, separatorArgs...))

	// Print rows
	for rowIndex, row := range t.rows {
		expandedRows := t.expandRow(row, rowIndex, widths)
		for expandedIdx, expandedRow := range expandedRows {
			// Print separator before first expanded line if this row position has a separator
			if expandedIdx == 0 && t.isSeparatorPosition(rowIndex) {
				separatorArgs := make([]interface{}, len(t.headers))
				for i := range t.headers {
					separatorArgs[i] = strings.Repeat("-", widths[i])
				}
				result.WriteString(fmt.Sprintf(formatStr, separatorArgs...))
			}

			rowArgs := make([]interface{}, len(t.headers))
			for i := range t.headers {
				if i < len(expandedRow) {
					rowArgs[i] = expandedRow[i]
				} else {
					rowArgs[i] = ""
				}
			}
			result.WriteString(fmt.Sprintf(formatStr, rowArgs...))
		}
	}

	return result.String()
}

// Ascii renders the table in ASCII format with borders
func (t *Table) Ascii() string {
	if len(t.headers) == 0 {
		return ""
	}

	widths := t.calculateWidths()
	var result strings.Builder

	// Build top border
	result.WriteString("+")
	for i, width := range widths {
		result.WriteString(strings.Repeat("-", width+2))
		if i < len(widths)-1 {
			result.WriteString("+")
		}
	}
	result.WriteString("+\n")

	// Print header
	result.WriteString("|")
	for i, header := range t.headers {
		coloredHeader := formatHeaderWithColor(header, widths[i])
		// Add padding spaces (width+2 total: 1 space before + width content + 1 space after)
		result.WriteString(" ")
		result.WriteString(coloredHeader)
		result.WriteString(" ")
		result.WriteString("|")
	}
	result.WriteString("\n")

	// Print separator after header
	result.WriteString("+")
	for i, width := range widths {
		result.WriteString(strings.Repeat("-", width+2))
		if i < len(widths)-1 {
			result.WriteString("+")
		}
	}
	result.WriteString("+\n")

	// Print rows
	for rowIndex, row := range t.rows {
		expandedRows := t.expandRow(row, rowIndex, widths)
		for expandedIdx, expandedRow := range expandedRows {
			// Print separator before first expanded line if this row position has a separator
			if expandedIdx == 0 && t.isSeparatorPosition(rowIndex) {
				result.WriteString("+")
				for i, width := range widths {
					result.WriteString(strings.Repeat("-", width+2))
					if i < len(widths)-1 {
						result.WriteString("+")
					}
				}
				result.WriteString("+\n")
			}

			result.WriteString("|")
			for i := range t.headers {
				value := ""
				if i < len(expandedRow) {
					value = expandedRow[i]
				}
				result.WriteString(fmt.Sprintf(" %-*s ", widths[i], value))
				result.WriteString("|")
			}
			result.WriteString("\n")
		}
	}

	// Print bottom border
	result.WriteString("+")
	for i, width := range widths {
		result.WriteString(strings.Repeat("-", width+2))
		if i < len(widths)-1 {
			result.WriteString("+")
		}
	}
	result.WriteString("+\n")

	return result.String()
}

// Text renders the table as simple text, line by line
func (t *Table) Text() string {
	if len(t.headers) == 0 {
		return ""
	}

	widths := t.calculateWidths()
	var result strings.Builder

	// Print header
	coloredHeaders := make([]string, len(t.headers))
	for i, header := range t.headers {
		coloredHeaders[i] = colorGreenHeader(header)
	}
	result.WriteString(strings.Join(coloredHeaders, " "))
	result.WriteString(strings.Repeat(" ", len(t.headers)) + "\n")

	// Print rows
	for rowIndex, row := range t.rows {
		expandedRows := t.expandRow(row, rowIndex, widths)
		for expandedIdx, expandedRow := range expandedRows {
			// Print separator (just newline) before first expanded line if this row position has a separator
			if expandedIdx == 0 && t.isSeparatorPosition(rowIndex) {
				result.WriteString("\n")
			}

			result.WriteString(strings.Join(expandedRow, " "))
			result.WriteString("\n")
		}
	}

	return result.String()
}
