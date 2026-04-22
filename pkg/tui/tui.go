package tui

import (
	"go.gokku-vm.com/pkg/tui/table"
)

const (
	ASCII = "ascii"
	TEXT  = "text"
	TABLE = "table"
)

func NewTable(tableType string) *table.Table {
	return table.NewTable(tableType)
}
