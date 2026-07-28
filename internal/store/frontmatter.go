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
