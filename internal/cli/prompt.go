package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// huhPrompter renders the setup questions as an interactive form.
//
// It is intentionally thin: it turns a question into a widget and returns the
// answer. Everything that could be wrong (validation, defaults, the files that
// result) lives in package wizard and is covered by tests.
type huhPrompter struct {
	out   *output
	theme *huh.Theme
	// in and out override the terminal, which is what lets the real forms be
	// driven by scripted keystrokes in tests.
	in io.Reader
	w  io.Writer
}

func newHuhPrompter(out *output) *huhPrompter {
	return &huhPrompter{out: out, theme: huh.ThemeCharm()}
}

func (p *huhPrompter) Section(title, desc string) {
	p.out.heading(title)
	if desc != "" {
		p.out.printf("  %s\n", desc)
	}
}

func (p *huhPrompter) Input(label, desc, def string, validate func(string) error) (string, error) {
	value := def
	field := huh.NewInput().Title(label).Value(&value)
	// The default is pre-filled and editable; saying so removes the doubt about
	// whether pressing enter accepts it.
	if hint := defaultHint(desc, def); hint != "" {
		field = field.Description(hint)
	}
	if def != "" {
		field = field.Placeholder(def)
	}
	if validate != nil {
		field = field.Validate(validate)
	}
	if err := p.run(field); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func (p *huhPrompter) Select(label, desc string, options []wizard.Option, def string) (string, error) {
	value := def
	opts := make([]huh.Option[string], 0, len(options))
	for _, o := range options {
		// The label is the whole option: huh puts every option on one line, so
		// a description appended here reads as part of the choice itself.
		opts = append(opts, huh.NewOption(o.Label, o.Value))
	}
	field := huh.NewSelect[string]().Title(label).Options(opts...).Value(&value)
	if desc != "" {
		field = field.Description(desc)
	}
	if err := p.run(field); err != nil {
		return "", err
	}
	return value, nil
}

func (p *huhPrompter) Confirm(label, desc string, def bool) (bool, error) {
	value := def
	field := huh.NewConfirm().Title(label).Value(&value)
	if desc != "" {
		field = field.Description(desc)
	}
	if err := p.run(field); err != nil {
		return false, err
	}
	return value, nil
}

func (p *huhPrompter) run(field huh.Field) error {
	form := huh.NewForm(huh.NewGroup(field)).WithTheme(p.theme)
	if p.in != nil {
		form = form.WithInput(p.in)
	}
	if p.w != nil {
		form = form.WithOutput(p.w)
	}
	err := form.Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return errCancelled
	}
	return err
}

var errCancelled = errors.New("setup cancelled")

// defaultHint appends the "what happens if I just press enter" line that every
// good installer shows next to a pre-filled answer.
func defaultHint(desc, def string) string {
	var hint string
	switch {
	case def != "":
		hint = "Press enter to keep " + def + "."
	default:
		hint = "Press enter to leave this empty."
	}
	if desc == "" {
		return hint
	}
	return desc + "\n" + hint
}

// flagPrompter answers from pre-supplied values instead of asking. It backs
// --non-interactive, so CI pipelines and agents run the very same wizard code
// as a human at a terminal.
type flagPrompter struct {
	out *output
}

func (p *flagPrompter) Section(title, _ string) {
	if p.out != nil {
		p.out.heading(title)
	}
}

func (p *flagPrompter) Input(label, _, def string, validate func(string) error) (string, error) {
	if validate != nil {
		if err := validate(def); err != nil {
			return "", fmt.Errorf("%s: %w (pass it with the matching flag)", label, err)
		}
	}
	if p.out != nil {
		p.out.skip("%s: %s", label, displayValue(def))
	}
	return strings.TrimSpace(def), nil
}

func (p *flagPrompter) Select(label, _ string, options []wizard.Option, def string) (string, error) {
	for _, o := range options {
		if o.Value == def {
			if p.out != nil {
				p.out.skip("%s: %s", label, o.Label)
			}
			return def, nil
		}
	}
	valid := make([]string, 0, len(options))
	for _, o := range options {
		valid = append(valid, o.Value)
	}
	return "", fmt.Errorf("%s: %q is not one of %s", label, def, strings.Join(valid, ", "))
}

func (p *flagPrompter) Confirm(label, _ string, def bool) (bool, error) {
	if p.out != nil {
		p.out.skip("%s: %t", label, def)
	}
	return def, nil
}

func displayValue(v string) string {
	if v == "" {
		return "(empty)"
	}
	return v
}
