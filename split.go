package main

import "strings"

// Para is a blank-line-separated block. Line numbers are 1-based inclusive.
type Para struct {
	Seq       int
	StartLine int
	EndLine   int
	Text      string
}

func splitParas(content string) []Para {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var paras []Para
	start := -1
	var buf []string
	flush := func(end int) {
		if start >= 0 {
			paras = append(paras, Para{
				Seq: len(paras), StartLine: start + 1, EndLine: end,
				Text: strings.Join(buf, "\n"),
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
		}
	}
	flush(len(lines))
	return paras
}
