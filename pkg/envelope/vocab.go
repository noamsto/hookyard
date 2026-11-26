package envelope

import "github.com/noamsto/hookyard/internal/vocab"

// Engine names the engine that sent a payload. It is an alias of hookyard's
// internal vocabulary type so external importers can name and compare
// Envelope.Engine without importing an internal package.
type Engine = vocab.Engine

// The engines Detect can place.
const (
	ClaudeCode = vocab.ClaudeCode
	Codex      = vocab.Codex
	Cursor     = vocab.Cursor
	Pi         = vocab.Pi
)

// The canonical events Envelope.CanonicalEvent carries.
const (
	SessionStart = vocab.SessionStart
	PromptSubmit = vocab.PromptSubmit
	PreTool      = vocab.PreTool
	PostTool     = vocab.PostTool
	PreCompact   = vocab.PreCompact
	TurnEnd      = vocab.TurnEnd
)

// Engines lists every Engine in a stable order.
func Engines() []Engine { return append([]Engine(nil), vocab.Engines...) }

// CanonicalEvents lists every canonical event in a stable order.
func CanonicalEvents() []string { return append([]string(nil), vocab.CanonicalEvents...) }
