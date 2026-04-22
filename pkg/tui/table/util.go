package table

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// calculateWidths calculates the width for each column
func (t *Table) calculateWidths() []int {
	if len(t.headers) == 0 {
		t.headers = []string{}
	}

	widths := make([]int, len(t.headers))

	for i, header := range t.headers {
		widths[i] = len(header)
	}

	// Calculate widths based on expanded/truncated row data
	maxLineLen := getMaxLineLength()

	for rowIndex, row := range t.rows {
		shouldTruncate := t.truncate[rowIndex]

		if shouldTruncate {
			// For truncated rows, use original cell length for width calculation
			// Truncation happens later during rendering
			for i, cell := range row {
				if i < len(widths) {
					cellLen := len(strings.TrimSpace(cell))
					if cellLen > widths[i] {
						widths[i] = cellLen
					}
				}
			}
		} else {
			// For non-truncated rows, use original cell length
			for i, cell := range row {
				if i < len(widths) {
					cellLen := len(strings.TrimSpace(cell))
					if cellLen > widths[i] {
						widths[i] = cellLen
					}
				}
			}
		}
	}

	// If we have any truncated rows, ensure total width doesn't exceed terminal width
	hasTruncatedRows := false
	for _, truncated := range t.truncate {
		if truncated {
			hasTruncatedRows = true
			break
		}
	}

	if hasTruncatedRows && len(widths) > 0 {
		// Calculate total width (including spaces between columns)
		totalWidth := 0
		for _, w := range widths {
			totalWidth += w
		}
		// Add spaces between columns (len(widths) - 1 spaces)
		totalWidth += len(widths) - 1

		// Use more of terminal width when truncating (85%)
		terminalWidth, _, err := term.GetSize(int(os.Stdout.Fd()))
		maxAllowedWidth := maxLineLen
		if err == nil && terminalWidth > 0 {
			maxAllowedWidth = int(float64(terminalWidth) * 0.80)
		}

		// Only down if total significantly exceeds terminal width
		// Truncate only the largest column that exceeds space
		if totalWidth > maxAllowedWidth {
			// Find the largest column
			maxColIndex := 0
			maxColWidth := widths[0]
			for i, w := range widths {
				if w > maxColWidth {
					maxColWidth = w
					maxColIndex = i
				}
			}

			// Calculate space needed for other columns + spaces
			otherColumnsWidth := 0
			for i, w := range widths {
				if i != maxColIndex {
					otherColumnsWidth += w
				}
			}
			spacesWidth := len(widths) - 1
			availableForMaxCol := maxAllowedWidth - otherColumnsWidth - spacesWidth

			// Only truncate the largest column if needed
			if availableForMaxCol > 0 && widths[maxColIndex] > availableForMaxCol {
				widths[maxColIndex] = availableForMaxCol
			}
		}
	}

	for i := range widths {
		if widths[i] < 1 {
			widths[i] = 1
		}
	}

	return widths
}

// isSeparatorPosition checks if a row index should have a separator
func (t *Table) isSeparatorPosition(rowIndex int) bool {
	for _, sepPos := range t.separators {
		if sepPos == rowIndex {
			return true
		}
	}
	return false
}

// getMaxLineLength gets the maximum line length based on terminal width
func getMaxLineLength() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		// If we can't get terminal size, use default
		return DEFAULT_MAX_LINE_LENGTH
	}

	// Use terminal width, but ensure minimum of 80
	if width < 100 {
		return 100
	}

	// Reserve some space for formatting (spaces, borders, etc)
	// Use 95% of terminal width
	maxLen := int(float64(width) * 0.95)

	// Ensure reasonable bounds
	if maxLen < 100 {
		return 100
	}

	return maxLen
}

// isTerminal checks if stdout is a terminal
func isTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// colorGreen wraps text with green ANSI color codes if terminal supports it
func colorGreenHeader(text string) string {
	if !isTerminal() {
		return text
	}
	return colorGreen + text + colorReset
}

// formatHeaderWithColor formats a header with specified width and applies green color
func formatHeaderWithColor(header string, width int) string {
	formatted := fmt.Sprintf("%-*s", width, header)
	if !isTerminal() {
		return formatted
	}
	// Apply color to the text part (without trailing spaces)
	trimmed := strings.TrimRight(formatted, " ")
	if len(trimmed) == 0 {
		return formatted
	}
	return colorGreen + trimmed + colorReset + strings.Repeat(" ", len(formatted)-len(trimmed))
}

// breakText breaks text into lines of max characters
func breakText(text string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = getMaxLineLength()
	}

	text = strings.TrimSpace(text)
	if len(text) <= maxLen {
		return []string{text}
	}

	var lines []string
	for len(text) > maxLen {
		lines = append(lines, text[:maxLen])
		text = text[maxLen:]
	}
	if len(text) > 0 {
		lines = append(lines, text)
	}

	return lines
}

// truncateCell truncates a cell to fit within maxLen characters
func truncateCell(cell string, maxLen int) string {
	if maxLen <= 0 {
		return cell
	}
	cell = strings.TrimSpace(cell)
	if len(cell) <= maxLen {
		return cell
	}
	if maxLen <= 3 {
		return cell[:maxLen]
	}
	return cell[:maxLen-3] + "..."
}

// expandRow breaks cells that exceed max characters into multiple lines, or truncates if truncate is true
func (t *Table) expandRow(row []string, rowIndex int, widths []int) [][]string {
	if len(row) == 0 {
		return [][]string{}
	}

	shouldTruncate := t.truncate[rowIndex]

	if shouldTruncate {
		// Truncate mode: truncate each cell based on its column width
		expandedRow := make([]string, len(row))

		for i, cell := range row {
			// Use column width for truncation
			maxLen := getMaxLineLength()
			if i < len(widths) && widths[i] > 0 {
				maxLen = widths[i]
			}

			// Only truncate if cell exceeds column width
			cellLen := len(strings.TrimSpace(cell))
			if cellLen > maxLen {
				expandedRow[i] = truncateCell(cell, maxLen)
			} else {
				expandedRow[i] = strings.TrimSpace(cell)
			}
		}
		return [][]string{expandedRow}
	}

	// Normal mode: break into multiple lines
	maxLines := 1
	cellLines := make([][]string, len(row))

	for i, cell := range row {
		cell = strings.TrimSpace(cell)
		lines := breakText(cell, getMaxLineLength())
		cellLines[i] = lines
		if len(lines) > maxLines {
			maxLines = len(lines)
		}
	}

	expandedRows := make([][]string, maxLines)
	for lineIdx := 0; lineIdx < maxLines; lineIdx++ {
		expandedRows[lineIdx] = make([]string, len(row))
		for colIdx := 0; colIdx < len(row); colIdx++ {
			if lineIdx < len(cellLines[colIdx]) {
				expandedRows[lineIdx][colIdx] = cellLines[colIdx][lineIdx]
			} else {
				// If this is a continuation line (lineIdx > 0), leave other columns blank
				// Otherwise, if it's the first line and cell wasn't broken, use original value
				if lineIdx == 0 {
					expandedRows[lineIdx][colIdx] = strings.TrimSpace(row[colIdx])
				} else {
					expandedRows[lineIdx][colIdx] = ""
				}
			}
		}
	}

	return expandedRows
}
