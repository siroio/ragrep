package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/siroio/ragrep/internal/store"
)

// evalCase is one line of a JSONL eval file. Para is a pointer, not a plain
// int: store.Hit.Para is a 0-based paragraph seq, so 0 is a valid seq value
// and can't double as "absent". nil means doc-level match (any paragraph);
// non-nil means the hit's paragraph must equal *Para exactly.
type evalCase struct {
	Query string `json:"query"`
	Doc   string `json:"doc"`
	Para  *int   `json:"para"`
}

// readEvalCases parses a JSONL eval file, skipping blank lines. A malformed
// line is a hard error rather than a skipped case -- silently dropping a
// typo'd case would understate recall in a way that's invisible to the user.
func readEvalCases(path string) ([]evalCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cases []evalCase
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c evalCase
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("parsing eval case %q: %w", line, err)
		}
		cases = append(cases, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return cases, nil
}

// recalled reports whether hits satisfy c: any hit in the wanted doc, and
// (when c.Para is non-nil) in the wanted paragraph too.
func recalled(hits []store.Hit, c evalCase) bool {
	for _, h := range hits {
		if h.Doc == c.Doc && (c.Para == nil || h.Para == *c.Para) {
			return true
		}
	}
	return false
}

// cmdEval reads JSONL eval cases and reports recall@k: the fraction of cases
// where a top-k search hit landed in the case's wanted doc (and paragraph,
// if given). Exit 0 whenever the file parsed and had cases -- a bad recall
// score is a measurement, not a CLI error. Exit 1 on an unreadable/malformed
// file or an empty case set.
func cmdEval(args []string) int {
	fs := newFlagSet("eval")
	db := dbFlag(fs)
	mode := fs.String("mode", "hybrid", "hybrid|vector|text")
	k := fs.Int("k", 10, "max results per query")
	if code, handled := parseArgs(fs, args); handled {
		return code
	}
	if fs.NArg() != 1 {
		return fail(fmt.Errorf("usage: ragrep eval <cases.jsonl>"))
	}

	cases, err := readEvalCases(fs.Arg(0))
	if err != nil {
		return fail(err)
	}
	if len(cases) == 0 {
		return fail(fmt.Errorf("no eval cases in %s", fs.Arg(0)))
	}

	s, err := openStoreAt(*db)
	if err != nil {
		return fail(err)
	}
	defer s.Close()

	hit := 0
	for _, c := range cases {
		hits, err := runSearch(s, *mode, c.Query, *k, nil)
		if err != nil {
			return fail(err)
		}
		if recalled(hits, c) {
			hit++
		} else {
			fmt.Printf("miss: %q want %s\n", c.Query, c.Doc)
		}
	}
	fmt.Printf("recall@%d: %.3f (%d/%d)\n", *k, float64(hit)/float64(len(cases)), hit, len(cases))
	return 0
}
