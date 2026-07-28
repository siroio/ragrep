// Package sample is fixture data for codeindex tests. It lives under
// testdata, so `go build`/`go vet` never compile it as part of any package.
package sample

import "context"

// Store persists items keyed by ID. 日本語のコメントも含みます。
type Store struct {
	// Name identifies the store.
	Name string
}

// Save writes item to the store and reports any failure. 🎉
func (s *Store) Save(ctx context.Context, item string) error {
	return nil
}

// Load reads the item with the given id.
func (s *Store) Load(ctx context.Context, id string) (string, error) {
	return "", nil
}
