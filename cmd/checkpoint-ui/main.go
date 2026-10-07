// Command checkpoint-ui is the terminal screen behind `checkpoint ui`.
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/manyapn/checkpoint-public/internal/tui"
)

func main() {
	cli := flag.String("cli", "checkpoint", "path to the checkpoint binary")
	root := flag.String("root", "", "protected folder")
	store := flag.String("store", "", "store directory")
	flag.Parse()
	if _, err := tea.NewProgram(tui.New(*cli, *root, *store)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
