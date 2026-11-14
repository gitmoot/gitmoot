// Package skills embeds the gitmoot Agent Skill (skills/gitmoot), the one
// source for the codex and claude plugin packages and for gitmoot.io/SKILL.md.
package skills

import "embed"

// gitmoot/references/CLI.md is the binary's own --help output; CI re-runs
// `go generate ./...` and fails when the checked-in copy is stale.
//
//go:generate go run ../internal/clidoc/clidocgen -out gitmoot/references/CLI.md

// FS contains the canonical Gitmoot skill package used for generated plugins.
//
//go:embed gitmoot/**
var FS embed.FS
