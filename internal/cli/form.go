package cli

import (
	"errors"
	"io"
	"sync"

	"github.com/charmbracelet/huh"

	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// runForm asks the whole wizard as a single interactive form.
//
// One form, not one per question, and the reason is not tidiness. Every
// Bubble Tea program asks the terminal what colour its background is; the
// reply arrives as input, and with a program per question it lands in the
// next question's field, which is why answering used to take two presses of
// enter and sometimes left an escape sequence in the box. One program asks
// once.
//
// The questions come from wizard.Questions(), the same list the non
// interactive path walks, so the two cannot ask different things.
func runForm(in io.Reader, w io.Writer, a wizard.Answers) (wizard.Answers, error) {
	state := &formState{answers: a}

	var groups []*huh.Group
	section := ""
	for _, q := range wizard.Questions() {
		group := huh.NewGroup(fieldFor(q, state))
		if q.Section != section {
			section = q.Section
			group = group.Title(q.Section)
		}
		// Every group tidies the answers before deciding whether it applies,
		// so the certificate question sees https://files.example.com rather
		// than the files.example.com that was typed one group earlier.
		group = group.WithHideFunc(func() bool { return !q.Applies(state.settled()) })
		groups = append(groups, group)
	}

	form := huh.NewForm(groups...).WithTheme(huh.ThemeCharm())
	if in != nil {
		form = form.WithInput(in)
	}
	if w != nil {
		form = form.WithOutput(w)
	}
	err := form.Run()
	switch {
	case errors.Is(err, huh.ErrUserAborted):
		return state.settled(), errCancelled
	case err != nil:
		return state.settled(), err
	}
	return state.settled(), nil
}

// formState holds the answers while the form is running.
//
// It needs a lock: huh updates a field on its event loop but evaluates the
// description and option functions in a command goroutine, so the answers are
// read and written from two goroutines at once. Every access goes through
// this, which is why the fields are bound with an accessor rather than with a
// bare pointer.
type formState struct {
	mu      sync.Mutex
	answers wizard.Answers
}

// snapshot is a copy of the answers as they stand.
func (s *formState) snapshot() wizard.Answers {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answers
}

// settled is the answers with every question's tidying applied, which is what
// the questions further down are asked about.
func (s *formState) settled() wizard.Answers {
	s.mu.Lock()
	defer s.mu.Unlock()
	wizard.Finalise(&s.answers)
	return s.answers
}

// guarded is a huh.Accessor for one field of the shared answers.
type guarded[T any] struct {
	state *formState
	field func(*wizard.Answers) *T
}

func (g *guarded[T]) Get() T {
	g.state.mu.Lock()
	defer g.state.mu.Unlock()
	return *g.field(&g.state.answers)
}

func (g *guarded[T]) Set(value T) {
	g.state.mu.Lock()
	defer g.state.mu.Unlock()
	*g.field(&g.state.answers) = value
}

// fieldFor turns one question into one widget.
func fieldFor(q wizard.Question, s *formState) huh.Field {
	describe := func() string { return q.Describe(s.snapshot()) }
	switch q.Kind {
	case wizard.KindSelect:
		return huh.NewSelect[string]().
			Title(q.Label).
			DescriptionFunc(describe, &s.answers).
			OptionsFunc(func() []huh.Option[string] { return optionsFor(q, s) }, &s.answers).
			Accessor(&guarded[string]{state: s, field: q.Str})
	case wizard.KindConfirm:
		return huh.NewConfirm().
			Title(q.Label).
			DescriptionFunc(describe, &s.answers).
			Accessor(&guarded[bool]{state: s, field: q.Bool})
	default:
		field := huh.NewInput().
			Title(q.Label).
			DescriptionFunc(describe, &s.answers).
			Accessor(&guarded[string]{state: s, field: q.Str})
		// The placeholder shows the default that pressing enter accepts.
		if answers := s.snapshot(); *q.Str(&answers) != "" {
			field = field.Placeholder(*q.Str(&answers))
		}
		if q.Validate != nil {
			field = field.Validate(q.Validate)
		}
		return field
	}
}

// optionsFor renders a question's choices. It only reads: this runs in a
// command goroutine, and wizard.Finalise, on the event loop, is what moves an
// answer off an option that is no longer offered.
func optionsFor(q wizard.Question, s *formState) []huh.Option[string] {
	answers := s.snapshot()
	choices := q.Options(answers)
	opts := make([]huh.Option[string], 0, len(choices))
	for _, o := range choices {
		opts = append(opts, huh.NewOption(o.Label, o.Value))
	}
	return opts
}
