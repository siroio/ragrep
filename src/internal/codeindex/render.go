package codeindex

import "strings"

// RenderEmbeddingText builds the canonical, deterministic text handed to
// the embedding model for one symbol: one "field: value" line per
// non-empty field, in a fixed order (language, kind, qualified_name,
// container, signature, documentation, path). Empty fields are omitted
// entirely rather than rendered as blank lines.
//
// Only declaration-level facts are embedded — never a symbol's body — so a
// struct/class record never carries its methods' bodies; each method is
// already its own separate Symbol (see extract.go's flatten).
func RenderEmbeddingText(s Symbol) string {
	var b strings.Builder
	writeField(&b, "language", s.Language)
	writeField(&b, "kind", s.Kind)
	writeField(&b, "qualified_name", s.QualifiedName)
	writeField(&b, "container", s.Container)
	writeField(&b, "signature", s.Signature)
	writeField(&b, "documentation", s.Documentation)
	writeField(&b, "path", s.Path)
	return strings.TrimSuffix(b.String(), "\n")
}

func writeField(b *strings.Builder, name, value string) {
	if value == "" {
		return
	}
	b.WriteString(name)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\n")
}
