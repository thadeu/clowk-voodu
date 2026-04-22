package v1

import (
	"encoding/json"
	"fmt"
	"os"

	"go.gokku-vm.com/pkg/tui"
)

// OutputFormat define o tipo de formato de saída
type OutputFormat string

const (
	// OutputFormatJSON formato JSON para APIs
	OutputFormatJSON OutputFormat = "json"
	// OutputFormatStdout formato padrão para CLI
	OutputFormatStdout OutputFormat = "stdout"
)

// Output é a interface para diferentes formatos de saída
type Output interface {
	// Success imprime uma mensagem de sucesso
	Success(message string)
	// Error imprime uma mensagem de erro
	Error(message string)
	// Data imprime dados estruturados
	Data(data interface{})
	// Table imprime uma tabela (apenas para stdout)
	Table(headers []string, rows [][]string)
	// Print imprime uma mensagem simples
	Print(message string)
}

// JSONOutput implementa Output para formato JSON
type JSONOutput struct{}

// StdoutOutput implementa Output para formato stdout (CLI)
type StdoutOutput struct{}

// Response é a estrutura de resposta JSON
type Response struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// NewOutput cria uma nova instância de Output baseada no formato
func NewOutput(format OutputFormat) Output {
	switch format {
	case OutputFormatJSON:
		return &JSONOutput{}
	case OutputFormatStdout:
		return &StdoutOutput{}
	default:
		return &StdoutOutput{}
	}
}

// JSONOutput implementations

func (o *JSONOutput) Success(message string) {
	resp := Response{
		Success: true,
		Message: message,
	}
	o.printJSON(resp)
}

func (o *JSONOutput) Error(message string) {
	resp := Response{
		Success: false,
		Error:   message,
	}
	o.printJSON(resp)
	os.Exit(1)
}

func (o *JSONOutput) Data(data interface{}) {
	resp := Response{
		Success: true,
		Data:    data,
	}

	o.printJSON(resp)
}

func (o *JSONOutput) Table(headers []string, rows [][]string) {
	// Para JSON, convertemos a tabela em array de objetos
	var result []map[string]string

	for _, row := range rows {
		item := make(map[string]string)

		for i, header := range headers {
			if i < len(row) {
				item[header] = row[i]
			}
		}

		result = append(result, item)
	}

	o.Data(result)
}

func (o *JSONOutput) Print(message string) {
	resp := Response{
		Success: true,
		Message: message,
	}

	o.printJSON(resp)
}

func (o *JSONOutput) printJSON(resp Response) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.Encode(resp)
}

func (o *StdoutOutput) Success(message string) {
	fmt.Printf("✓ %s\n", message)
}

func (o *StdoutOutput) Error(message string) {
	fmt.Fprintf(os.Stderr, "Error: %s\n", message)
	os.Exit(1)
}

func (o *StdoutOutput) Data(data interface{}) {
	// Para stdout, tentamos imprimir de forma legível
	switch v := data.(type) {
	case string:
		fmt.Println(v)
	case map[string]string:
		for key, value := range v {
			fmt.Printf("%s=%s\n", key, value)
		}
	default:
		// Fallback para JSON pretty print
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		encoder.Encode(data)
	}
}

func (o *StdoutOutput) Table(headers []string, rows [][]string) {
	table := tui.NewTable(tui.TABLE)
	table.AppendHeaders(headers)

	for _, row := range rows {
		table.AppendRow(row)
	}

	o.Print(table.Render())
}

func (o *StdoutOutput) Print(message string) {
	fmt.Println(message)
}
