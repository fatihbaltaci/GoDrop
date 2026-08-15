package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// output centralises how a command talks to the user: coloured and spaced for a
// terminal, plain for a pipe, or JSON when asked.
type output struct {
	w     io.Writer
	json  bool
	color bool
}

func newOutput(cmd *cobra.Command) *output {
	jsonOut, _ := cmd.Flags().GetBool("json")
	noColor, _ := cmd.Flags().GetBool("no-color")
	w := cmd.OutOrStdout()
	return &output{w: w, json: jsonOut, color: useColor(w) && !noColor && !jsonOut}
}

// useColor reports whether colour escapes are appropriate: a real terminal, no
// NO_COLOR, no CI marker. Agents and log files get plain text.
func useColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// interactive reports whether we may draw a full-screen form: both ends must be
// a terminal, otherwise the wizard would hang forever in CI or under an agent.
// It is a variable so both paths can be tested.
var interactive = func() bool { return interactiveOn(os.Stdin, os.Stdout) }

func interactiveOn(files ...*os.File) bool {
	for _, f := range files {
		info, err := f.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	return os.Getenv("CI") == "" && os.Getenv("TERM") != "dumb"
}

func (o *output) emit(v any) error {
	enc := json.NewEncoder(o.w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printf writes prose for a human. In --json mode it writes nothing at all:
// anything else would interleave with the machine-readable document and break
// the caller's parser.
func (o *output) printf(format string, args ...any) {
	if o.json {
		return
	}
	fmt.Fprintf(o.w, format, args...)
}

func (o *output) tint(c *color.Color, s string) string {
	if !o.color {
		return s
	}
	return c.Sprint(s)
}

func (o *output) success(format string, args ...any) {
	o.printf("  %s %s\n", o.tint(color.New(color.FgGreen), "✓"), fmt.Sprintf(format, args...))
}

func (o *output) warn(format string, args ...any) {
	o.printf("  %s %s\n", o.tint(color.New(color.FgYellow), "⚠"), fmt.Sprintf(format, args...))
}

func (o *output) fail(format string, args ...any) {
	o.printf("  %s %s\n", o.tint(color.New(color.FgRed), "✗"), fmt.Sprintf(format, args...))
}

func (o *output) skip(format string, args ...any) {
	o.printf("  %s %s\n", o.tint(color.New(color.Faint), "-"), fmt.Sprintf(format, args...))
}

func (o *output) heading(s string) {
	o.printf("\n%s\n", o.tint(color.New(color.Bold), s))
}

func (o *output) hint(format string, args ...any) {
	o.printf("      %s %s\n", o.tint(color.New(color.FgCyan), "→"), fmt.Sprintf(format, args...))
}

func (o *output) command(s string) {
	o.printf("      %s\n", o.tint(color.New(color.FgCyan), s))
}

func errorPrefix() string { return errorPrefixFor(os.Stderr) }

func errorPrefixFor(w io.Writer) string {
	if useColor(w) {
		return color.New(color.FgRed, color.Bold).Sprint("error: ")
	}
	return "error: "
}

// box draws a framed block, used to make the generated token impossible to miss.
func (o *output) box(lines ...string) {
	width := 0
	for _, l := range lines {
		if n := len([]rune(l)); n > width {
			width = n
		}
	}
	border := strings.Repeat("─", width+2)
	o.printf("  ┌%s┐\n", border)
	for _, l := range lines {
		o.printf("  │ %s%s │\n", l, strings.Repeat(" ", width-len([]rune(l))))
	}
	o.printf("  └%s┘\n", border)
}
