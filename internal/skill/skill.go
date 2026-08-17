// Package skill holds the agent skill, once.
//
// Two things hand it out and they must hand out the same bytes: `godrop skill
// install`, which writes it into an agent's skill directory, and GET /skill.md,
// which lets an agent that knows nothing but the hostname install it with
// `npx skills add <base>/skill.md`. Keeping it in the command would mean the
// server importing the command, and the command already imports the server.
//
// The copy at skills/godrop/SKILL.md is the one people and skill installers
// read on GitHub. `make docs` copies it here and a test fails if they drift.
package skill

import _ "embed"

// Name is the directory the skill is installed as, and the name its
// frontmatter carries. The Agent Skills specification requires the two to
// match.
const Name = "godrop"

//go:embed SKILL.md
var Markdown string
