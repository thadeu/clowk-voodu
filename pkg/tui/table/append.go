package table

// AppendHeader adds a header to the table
func (t *Table) AppendHeader(header string) {
	t.headers = append(t.headers, header)
}

// AppendHeaders sets the headers for the table (replaces existing headers)
func (t *Table) AppendHeaders(headers []string) {
	t.headers = headers
}

// AppendRow adds a row to the table
// truncate: if true, truncates cells that exceed column width instead of wrapping
func (t *Table) AppendRow(row []string, truncate ...bool) {
	rowIndex := len(t.rows)
	t.rows = append(t.rows, row)

	shouldTruncate := false
	if len(truncate) > 0 {
		shouldTruncate = truncate[0]
	}

	if shouldTruncate {
		t.truncate[rowIndex] = true
	}
}

// AppendSeparator marks the next row position as a separator
func (t *Table) AppendSeparator() {
	t.separators = append(t.separators, len(t.rows))
}
