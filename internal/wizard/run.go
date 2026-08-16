package wizard

// defaultTLS is what the certificate question starts on: automatic whenever
// Let's Encrypt could issue for the name, since that is the answer that needs
// no further work from anyone. An answer that was given already, on the
// command line, is left alone.
func defaultTLS(a Answers) string {
	switch {
	case a.TLS != "":
		return a.TLS
	case CanAutoTLS(a.BaseURL):
		return TLSAuto
	case a.BaseURL == "":
		// A local run, where a certificate is neither wanted nor possible.
		return TLSNone
	default:
		// A name Let's Encrypt cannot issue for, so something in front of
		// GoDrop is the likeliest arrangement.
		return TLSProxy
	}
}

// Option is one choice offered by a Select prompt.
type Option struct {
	Label string
	Value string
	Desc  string
}

// Prompter is the interactive layer. Keeping it behind an interface means the
// wizard's logic can be exercised without a terminal, and that a non
// interactive run (CI, an agent, a piped shell) uses the very same code path.
type Prompter interface {
	Section(title, desc string)
	Input(label, desc, def string, validate func(string) error) (string, error)
	Select(label, desc string, options []Option, def string) (string, error)
	Confirm(label, desc string, def bool) (bool, error)
}

// Run asks every applicable question in order, starting from the supplied
// answers, which double as defaults so flags can pre-fill any of them.
//
// The questions come from Questions(), the same list the interactive form is
// built from, so the two cannot drift apart.
func Run(p Prompter, a Answers) (Answers, error) {
	section := ""
	for _, q := range Questions() {
		if !q.ask(a) {
			continue
		}
		if q.Section != section {
			section = q.Section
			p.Section(section, sectionDesc(section))
		}
		if err := askOne(p, q, &a); err != nil {
			return a, err
		}
	}
	Finalise(&a)
	return a, nil
}

// askOne puts a single question and records the answer.
func askOne(p Prompter, q Question, a *Answers) error {
	switch q.Kind {
	case KindInput:
		value, err := p.Input(q.Label, q.Describe(*a), *q.Str(a), q.Validate)
		if err != nil {
			return err
		}
		*q.Str(a) = value
	case KindSelect:
		value, err := p.Select(q.Label, q.Describe(*a), q.Options(*a), q.Default(*a))
		if err != nil {
			return err
		}
		*q.Str(a) = value
	}
	if q.Normalize != nil {
		q.Normalize(a)
	}
	return nil
}

// sectionDesc is the one line under a heading. A section with nothing to add
// says nothing.
func sectionDesc(section string) string {
	return map[string]string{
		"Public address": "Where will people reach this server?",
		"Service":        "How GoDrop should run on this machine.",
		"HTTPS":          "How this server gets its certificate.",
		"Storage":        "Where files are kept.",
		"Limits":         "How much, and for how long.",
	}[section]
}
