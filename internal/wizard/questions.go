package wizard

import "runtime"

// Kind is the shape of a question.
type Kind int

// The two shapes a question can take.
const (
	KindInput Kind = iota
	KindSelect
)

// Question describes one thing the wizard asks, as data rather than as code,
// so that the sequential prompter and the single interactive form ask exactly
// the same things in exactly the same order. Two implementations of one list
// can differ; two hand-written flows always do.
type Question struct {
	// Section groups questions under a heading.
	Section string
	Label   string
	// Desc may depend on the answers so far, which is how a question can name
	// the host the operator just typed.
	Desc func(a Answers) string
	Kind Kind
	// Ask reports whether the question is worth asking at all. A question with
	// only one possible answer is noise.
	Ask func(a Answers) bool
	// Options are the choices, for a select.
	Options func(a Answers) []Option
	// Validate rejects an answer, for an input.
	Validate func(string) error
	// Str points at the field this question fills in.
	Str func(a *Answers) *string
	// Normalize tidies the answer once it has been given.
	Normalize func(a *Answers)
}

// Questions is the whole conversation, in order.
//
// It is deliberately short. Every question here changes what gets written or
// how the service runs; everything else has a default that is right for almost
// everyone and can be edited in .env afterwards.
func Questions() []Question {
	return QuestionsFor(runtime.GOOS)
}

// QuestionsFor is Questions for a given platform, since the deployment styles
// on offer depend on the host.
func QuestionsFor(goos string) []Question {
	return []Question{
		{
			Section: "Public address",
			Label:   "Public URL",
			Desc: static("For example files.example.com, or https://files.example.com. Upload URLs " +
				"are built from this. Leave it empty for a local run."),
			Kind:      KindInput,
			Validate:  ValidateBaseURL,
			Str:       func(a *Answers) *string { return &a.BaseURL },
			Normalize: func(a *Answers) { a.BaseURL = NormalizeBaseURL(a.BaseURL) },
		},
		{
			Section: "Service",
			Label:   "How should it run?",
			Desc:    static("This decides what gets written next to your configuration."),
			Kind:    KindSelect,
			Options: func(Answers) []Option { return DeploymentOptions(goos) },
			Str:     func(a *Answers) *string { return &a.Deployment },
		},
		{
			Section: "HTTPS",
			Label:   "Certificate",
			Desc: func(a Answers) string {
				if CanAutoTLS(a.BaseURL) {
					return "GoDrop can obtain and renew one itself. That needs ports 443 and 80 " +
						"reachable, and " + hostOf(a.BaseURL) + " pointing at this machine."
				}
				return "Let's Encrypt cannot issue for this address, so the certificate has to " +
					"come from somewhere else."
			},
			Kind:    KindSelect,
			Ask:     AsksTLS,
			Options: func(a Answers) []Option { return TLSOptions(a.BaseURL) },
			Str:     func(a *Answers) *string { return &a.TLS },
		},
		{
			Section: "HTTPS",
			Label:   "Certificate file",
			Desc: static("The full chain, in PEM. Certbot calls it fullchain.pem, for example " +
				"/etc/letsencrypt/live/files.example.com/fullchain.pem."),
			Kind:     KindInput,
			Ask:      func(a Answers) bool { return a.TLS == TLSFile },
			Validate: ValidateFile,
			Str:      func(a *Answers) *string { return &a.TLSCert },
		},
		{
			Section:  "HTTPS",
			Label:    "Private key file",
			Desc:     static("In PEM, and readable only by the service. Certbot calls it privkey.pem."),
			Kind:     KindInput,
			Ask:      func(a Answers) bool { return a.TLS == TLSFile },
			Validate: ValidateFile,
			Str:      func(a *Answers) *string { return &a.TLSKey },
		},
		{
			Section: "Storage",
			Label:   "Data directory",
			Desc: static("An absolute path, for example /var/lib/godrop. Uploaded files live here, " +
				"so put it on the disk you intend to back up."),
			Kind:     KindInput,
			Ask:      NeedsDataDir,
			Validate: ValidateDir,
			Str:      func(a *Answers) *string { return &a.DataDir },
		},
		{
			Section: "Limits",
			Label:   "Settings",
			Desc: static("Whichever you pick, every value ends up in .env and can be changed " +
				"there later."),
			Kind:    KindSelect,
			Options: LimitsOptions,
			Str:     func(a *Answers) *string { return &a.Limits },
		},
		{
			Section:  "Limits",
			Label:    "Maximum file size",
			Desc:     static("For example 100MB, 2GB. Anything larger is rejected with 413."),
			Kind:     KindInput,
			Ask:      AdvancedLimits,
			Validate: ValidateSize,
			Str:      func(a *Answers) *string { return &a.MaxFileSize },
		},
		{
			Section: "Limits",
			Label:   "Storage quota",
			Desc: static("For example 20GB. Uploads stop with 507 once this much is stored. " +
				"Empty means unlimited, and a full disk takes the whole server down with it."),
			Kind:     KindInput,
			Ask:      AdvancedLimits,
			Validate: ValidateOptionalSize,
			Str:      func(a *Answers) *string { return &a.MaxTotalSize },
		},
		{
			Section: "Limits",
			Label:   "Delete files after",
			Desc: static("For example 30d, 12h. Empty keeps files until someone deletes them, and " +
				"each upload can still ask for its own expiry."),
			Kind:     KindInput,
			Ask:      AdvancedLimits,
			Validate: ValidateRetention,
			Str:      func(a *Answers) *string { return &a.Retention },
		},
		{
			Section:  "Limits",
			Label:    "Listen port",
			Desc:     static("For example 8747. The port GoDrop binds to on this machine."),
			Kind:     KindInput,
			Ask:      func(a Answers) bool { return AdvancedLimits(a) && !ServesTLS(a) },
			Validate: validateFreePort,
			Str:      func(a *Answers) *string { return &a.Port },
		},
	}
}

// Finalise settles the answers that a skipped question would otherwise leave
// stale: under docker compose there is no host directory at all, and a
// left-over default would create one nobody uses.
func Finalise(a *Answers) {
	Normalise(a)
	if !NeedsDataDir(*a) {
		a.DataDir = ""
	}
	if !AsksTLS(*a) {
		// Nothing was asked because there was nothing to ask: plain http.
		a.TLS = TLSNone
		return
	}
	switch {
	case a.TLS == "":
		// The certificate question has an answer before it is asked: the one
		// that needs no further work for this address.
		a.TLS = defaultTLS(*a)
	case a.TLS == TLSAuto && !CanAutoTLS(a.BaseURL):
		// An address can change while the wizard is running, and automatic
		// stops being on offer the moment it becomes one Let's Encrypt cannot
		// issue for. An answer nobody can choose cannot stand.
		a.TLS = TLSProxy
	}
}

// NeedsDataDir reports whether the operator has to choose where files live.
// Under docker compose they live in a named volume that docker manages, and
// asking for a path invites someone to point it at a directory the container
// cannot write to.
func NeedsDataDir(a Answers) bool { return a.Deployment != DeployCompose }

// ask reports whether a question applies, treating a missing Ask as yes.
func (q Question) ask(a Answers) bool { return q.Ask == nil || q.Ask(a) }

// Applies reports whether this question is worth putting to the operator.
func (q Question) Applies(a Answers) bool { return q.ask(a) }

// Describe renders the description for the answers so far.
func (q Question) Describe(a Answers) string {
	if q.Desc == nil {
		return ""
	}
	return q.Desc(a)
}

// Default is the answer a question starts on.
func (q Question) Default(a Answers) string {
	if q.Label == "Certificate" {
		return defaultTLS(a)
	}
	return *q.Str(&a)
}

// Normalise applies every question's tidying to the answers, which is
// idempotent, so it can be done as often as is convenient.
func Normalise(a *Answers) {
	for _, q := range Questions() {
		if q.Normalize != nil {
			q.Normalize(a)
		}
	}
}

func static(s string) func(Answers) string { return func(Answers) string { return s } }
