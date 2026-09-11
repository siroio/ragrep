package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDaemonRejectsOversizedJSONBeforeCallingService(t *testing.T) {
	called := false
	h := newDaemonHandler(fakeDaemonCodeService{search: func(context.Context, searchRequest) (searchResponse, error) {
		called = true
		return searchResponse{}, nil
	}}, "secret")
	body := `{"query":"Handler"}` + strings.Repeat(" ", 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/v1/code/search", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || called {
		t.Fatalf("status=%d called=%v body=%s, want bad request without service call", rr.Code, called, rr.Body.String())
	}
}

func TestDaemonRejectsUnauthorizedRequestBeforeReadingBody(t *testing.T) {
	body := &daemonBoundaryBody{Reader: bytes.NewReader([]byte(`{"query":"Handler"}`))}
	h := newDaemonHandler(fakeDaemonCodeService{search: func(context.Context, searchRequest) (searchResponse, error) {
		t.Fatal("unauthorized request reached service")
		return searchResponse{}, nil
	}}, "secret")
	req := httptest.NewRequest(http.MethodPost, "/v1/code/search", body)
	req.Body = body
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || body.reads != 0 {
		t.Fatalf("status=%d body reads=%d, want unauthorized with zero body reads", rr.Code, body.reads)
	}
}

func TestDaemonAcceptsLegitimateSearchWithinBodyLimit(t *testing.T) {
	var got searchRequest
	h := newDaemonHandler(fakeDaemonCodeService{search: func(_ context.Context, req searchRequest) (searchResponse, error) {
		got = req
		return searchResponse{Fresh: true, Generation: 7}, nil
	}}, "secret")
	req := httptest.NewRequest(http.MethodPost, "/v1/code/search", strings.NewReader(`{"root":"root","query":"Handler","k":3,"mode":"text"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || got.Root != "root" || got.Query != "Handler" || got.K != 3 || got.Mode != "text" {
		t.Fatalf("status=%d request=%+v body=%s, want successful search", rr.Code, got, rr.Body.String())
	}
}

type daemonBoundaryBody struct {
	*bytes.Reader
	reads int
}

func (b *daemonBoundaryBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}

func (b *daemonBoundaryBody) Close() error { return nil }

var _ io.ReadCloser = (*daemonBoundaryBody)(nil)
