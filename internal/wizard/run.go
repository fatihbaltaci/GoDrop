package wizard

import "runtime"

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

// Run asks every question in order, starting from the supplied answers (which
// double as defaults, so flags can pre-fill any of them).
func Run(p Prompter, a Answers) (Answers, error) {
	var err error

	p.Section("Public address", "Where will people reach this server?")
	if a.BaseURL, err = p.Input(
		"Public URL",
		"Leave empty to derive it from each request (fine for a quick local run, wrong behind a proxy).",
		a.BaseURL, ValidateBaseURL); err != nil {
		return a, err
	}

	p.Section("Storage", "Where files are kept, and how much space they may use.")
	if a.DataDir, err = p.Input("Data directory",
		"Uploaded files live here. Put it on the disk you intend to back up.",
		a.DataDir, ValidateDir); err != nil {
		return a, err
	}
	if a.MaxFileSize, err = p.Input("Maximum file size",
		"Rejected with 413 above this. Your reverse proxy must allow at least as much.",
		a.MaxFileSize, ValidateSize); err != nil {
		return a, err
	}
	if a.MaxTotalSize, err = p.Input("Storage quota",
		"Uploads stop with 507 once this much is stored. Empty means unlimited, and a full disk takes the whole server down.",
		a.MaxTotalSize, ValidateOptionalSize); err != nil {
		return a, err
	}
	if a.Retention, err = p.Input("Delete files after",
		"For example 30d. Empty keeps files forever.",
		a.Retention, ValidateRetention); err != nil {
		return a, err
	}

	p.Section("Service", "How GoDrop should run on this machine.")
	if a.Port, err = p.Input("Listen port", "The port GoDrop binds to locally.", a.Port, ValidatePort); err != nil {
		return a, err
	}
	if a.Deployment, err = p.Select("Deployment style", "",
		DeploymentOptions(runtime.GOOS), a.Deployment); err != nil {
		return a, err
	}

	p.Section("Finishing up", "")
	if a.Telemetry, err = p.Confirm("Send an anonymous daily heartbeat?",
		"Exactly this, once a day: {install_id, version, os, arch, deploy}. "+
			"No file names, no counters, no addresses. Turn it off any time with `godrop telemetry off`.",
		a.Telemetry); err != nil {
		return a, err
	}
	if a.ExternalCheck, err = p.Confirm("Verify the server is reachable from the internet?",
		"Asks godrop.sh to fetch your /healthz once. Only the URL is sent. "+
			"It is the only way to catch a cloud firewall, which is invisible from inside the machine.",
		a.ExternalCheck); err != nil {
		return a, err
	}
	return a, nil
}
