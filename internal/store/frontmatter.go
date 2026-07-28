package store

import "strings"

// ParseTags extracts the "tags" list from a leading YAML frontmatter block
// (--- ... ---). Supported forms: inline `tags: [a, b]` (brackets optional)
// and a block list of `- item` lines. Values are trimmed, unquoted,
// lowercased and deduplicated. Returns nil when there is no closed
// frontmatter block or no tags key.
// ponytail: hand-rolled 1-key parser, not YAML; add gopkg.in/yaml.v3 if more
// keys or nested values are ever needed.
func ParseTags(content string) []string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
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
