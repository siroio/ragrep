package store

import "strings"

// Para is a blank-line-separated block. Line numbers are 1-based inclusive.
type Para struct {
	Seq       int
	StartLine int
	EndLine   int
	Text      string
	Heading   string
}

// headingUpdate returns the crumb stack after seeing one line.
// "## Errors" at level 2 truncates the stack to 1 entry and appends "Errors".
func headingUpdate(crumbs []string, line string) []string {
	rest := strings.TrimLeft(line, "#")
	level := len(line) - len(rest)
	if level == 0 || level > 6 || !strings.HasPrefix(rest, " ") {
		return crumbs
	}
	text := strings.TrimSpace(rest)
	if text == "" {
		return crumbs
	}
	if level-1 < len(crumbs) {
		crumbs = crumbs[:level-1]
	}
	return append(crumbs, text)
}

func splitParas(content string) []Para {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var paras []Para
	var crumbs []string
	start := -1
	var buf []string
	flush := func(end int) {
		if start >= 0 {
			paras = append(paras, Para{
				Seq: len(paras), StartLine: start + 1, EndLine: end,
				Text:    strings.Join(buf, "\n"),
				Heading: strings.Join(crumbs, " > "),
			})
			start, buf = -1, nil
		}
	}
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			flush(i)
		} else {
			if start < 0 {
				start = i
			}
			buf = append(buf, ln)
			crumbs = headingUpdate(crumbs, ln)
		}
	}
	flush(len(lines))
	return paras
}
