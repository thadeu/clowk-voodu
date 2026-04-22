package table

const DEFAULT_MAX_LINE_LENGTH = 95

const (
	colorGreen = "\033[32m"
	colorReset = "\033[0m"
)

type Table struct {
	Type       string
	headers    []string
	rows       [][]string
	separators []int
	truncate   map[int]bool // Maps row index to truncate flag
}

// NewTable creates a new table with the specified type
func NewTable(tableType string) *Table {
	return &Table{
		Type:       tableType,
		headers:    []string{},
		rows:       [][]string{},
		separators: []int{},
		truncate:   make(map[int]bool),
	}
}
