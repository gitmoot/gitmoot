package skills

import "embed"

// FS contains the canonical Gitmoot skill package used for generated plugins.
//
//go:embed gitmoot/**
var FS embed.FS
