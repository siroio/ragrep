package store

import "strings"

// frontmatterEnd returns the 0-based index (into lines split on '\n') of the
// closing "---" delimiter of a leading frontmatter block, or -1 if lines[0]
// isn't "---" or the block is never closed.
func frontmatterEnd(lines []string) int {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return -1
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return i
		}
	}
	return -1
}

// frontmatterLineCount returns how many leading lines (1-based) a closed
// frontmatter block occupies, or 0 if content has none. Used to exclude the
// block itself from paragraph/search indexing (it's metadata, not content).
func frontmatterLineCount(content string) int {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	end := frontmatterEnd(lines)
	if end < 0 {
		return 0
	}
	return end + 1
}

// blankLines returns content with its first n lines replaced by empty
// lines. The total line count is unchanged, so line numbers computed over
// the result (e.g. splitParas's StartLine/EndLine) still match the original
// file; only used to hide the frontmatter block from paragraph splitting.
func blankLines(content string, n int) string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	for i := 0; i < n && i < len(lines); i++ {
		lines[i] = ""
	}
	return strings.Join(lines, "\n")
}

// ParseTags extracts the "tags" list from a leading YAML frontmatter block
// (--- ... ---). Supported forms: inline `tags: [a, b]` (brackets optional)
// and a block list of `- item` lines. Values are trimmed, unquoted,
// lowercased and deduplicated. Returns nil when there is no closed
// frontmatter block or no tags key.
// ponytail: hand-rolled 1-key parser, not YAML; add gopkg.in/yaml.v3 if more
// keys or nested values are ever needed.
func ParseTags(content string) []string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	end := frontmatterEnd(lines)
	if end < 0 {
		return nil
	}
	var tags []string
	seen := map[string]bool{}
	add := func(raw string) {
		t := strings.ToLower(strings.Trim(strings.TrimSpace(raw), `"'`))
		if t != "" && !seen[t] {
			seen[t] = true
			tags = append(tags, t)
		}
	}
	for i := 1; i < end; i++ {
		rest, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "tags:")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" { // block list on following lines
			for j := i + 1; j < end; j++ {
				item, ok := strings.CutPrefix(strings.TrimSpace(lines[j]), "- ")
				if !ok {
					break
				}
				add(item)
			}
		} else { // inline: [a, b] or a, b
			for _, part := range strings.Split(strings.Trim(rest, "[]"), ",") {
				add(part)
			}
		}
		break
	}
	return tags
}

// autoTags derives tags from a root-relative slash path: each directory
// segment plus the file extension, lowercased.
func autoTags(path string) []string {
	var tags []string
	segs := strings.Split(path, "/")
	for _, s := range segs[:len(segs)-1] {
		if s != "" {
			tags = append(tags, strings.ToLower(s))
		}
	}
	name := segs[len(segs)-1]
	if i := strings.LastIndexByte(name, '.'); i > 0 && i+1 < len(name) {
		tags = append(tags, strings.ToLower(name[i+1:]))
	}
	return tags
}
