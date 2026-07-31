package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/siroio/ragrep/internal/embed"
	"github.com/siroio/ragrep/internal/session"
	"github.com/siroio/ragrep/internal/store"
)

const sessionProtocolVersion = 1

type endpointRecord struct {
	DBPath           string `json:"db_path"`
	Address          string `json:"address"`
	ProtocolVersion  int    `json:"protocol_version"`
	ModelFingerprint string `json:"model_fingerprint"`
	Token            string `json:"token"`
}

type sessionRequest struct {
	Operation        string   `json:"op"`
	Token            string   `json:"token"`
	DBPath           string   `json:"db_path"`
	ModelFingerprint string   `json:"model_fingerprint"`
	Query            string   `json:"query,omitempty"`
	Mode             string   `json:"mode,omitempty"`
	K                int      `json:"k,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	Path             string   `json:"path,omitempty"`
	Lines            string   `json:"lines,omitempty"`
	Para             int      `json:"para,omitempty"`
	Context          int      `json:"context,omitempty"`
}

type sessionResponse struct {
	Hits     []store.Hit `json:"hits,omitempty"`
	Mtimes   []int64     `json:"mtimes,omitempty"`
	Content  string      `json:"content,omitempty"`
	NotFound bool        `json:"not_found,omitempty"`
	Error    string      `json:"error,omitempty"`
}

func cmdServe(args []string) int {
	fs := newFlagSet("serve")
	db := dbFlag(fs)
	endpoint := fs.String("endpoint", "", "path to the session endpoint record")
	idleTimeout := fs.Duration("idle-timeout", time.Minute, "shutdown after this idle duration")
	if code, handled := parseArgs(fs, args); handled {
		return code
	}
	if fs.NArg() != 0 || *endpoint == "" {
		return fail(fmt.Errorf("usage: ragrep serve --db PATH --endpoint PATH [--idle-timeout D]"))
	}
	if *idleTimeout <= 0 {
		return fail(fmt.Errorf("--idle-timeout must be positive"))
	}
	dbPath, err := filepath.Abs(*db)
	if err != nil {
		return fail(err)
	}
	s, err := openStoreAt(dbPath)
	if err != nil {
		return fail(err)
	}
	dir, err := embed.CacheDir()
	if err != nil {
		s.Close()
		return fail(err)
	}
	e, err := embed.New(dir)
	if err != nil {
		s.Close()
		return fail(err)
	}
	runtime := session.New(s, e)
	defer runtime.Close()
	fingerprint, err := embed.Fingerprint(dir)
	if err != nil {
		return fail(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail(err)
	}
	defer listener.Close()
	token, err := randomToken()
	if err != nil {
		return fail(err)
	}
	record := endpointRecord{dbPath, listener.Addr().String(), sessionProtocolVersion, fingerprint, token}
	if err := writeEndpointRecord(*endpoint, record); err != nil {
		return fail(err)
	}
	defer os.Remove(*endpoint)
	return serveSessionListener(listener, *idleTimeout, func(request sessionRequest) sessionResponse {
		if request.Token != token {
			return sessionResponse{Error: "invalid session token"}
		}
		if !samePath(request.DBPath, dbPath) {
			return sessionResponse{Error: "session database does not match request"}
		}
		if request.ModelFingerprint != fingerprint {
			return sessionResponse{Error: "session model fingerprint does not match request"}
		}
		switch request.Operation {
		case "search":
			hits, err := runtime.Search(request.Mode, request.Query, request.K, request.Tags)
			if err != nil {
				return sessionResponse{Error: err.Error()}
			}
			mtimes := make([]int64, len(hits))
			for i, hit := range hits {
				mtimes[i] = hit.Mtime
			}
			return sessionResponse{Hits: hits, Mtimes: mtimes}
		case "get":
			content, err := runtime.GetContent(request.Path, request.Lines, request.Para, request.Context)
			if errors.Is(err, store.ErrNotFound) {
				return sessionResponse{NotFound: true}
			}
			if err != nil {
				return sessionResponse{Error: err.Error()}
			}
			return sessionResponse{Content: content}
		default:
			return sessionResponse{Error: fmt.Sprintf("unknown operation %q", request.Operation)}
		}
	})
}

func serveSessionListener(listener net.Listener, idle time.Duration, handler func(sessionRequest) sessionResponse) int {
	for {
		if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(idle)); err != nil {
			return fail(err)
		}
		conn, err := listener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return 0
			}
			return fail(err)
		}
		serveSessionConnection(conn, handler)
	}
}

func serveSessionConnection(conn net.Conn, handler func(sessionRequest) sessionResponse) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	defer writer.Flush()
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var request sessionRequest
		response := sessionResponse{}
		if err := json.Unmarshal(line, &request); err != nil {
			response.Error = "malformed session request"
		} else if request.Operation != "search" && request.Operation != "get" {
			response.Error = fmt.Sprintf("unknown operation %q", request.Operation)
		} else {
			response = handler(request)
		}
		encoded, _ := json.Marshal(response)
		writer.Write(append(encoded, '\n'))
		writer.Flush()
	}
}

func writeEndpointRecord(path string, record endpointRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ragrep-session-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func readEndpointRecord(path string) (endpointRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return endpointRecord{}, err
	}
	var record endpointRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return endpointRecord{}, err
	}
	if record.ProtocolVersion != sessionProtocolVersion || record.DBPath == "" || record.Address == "" || record.ModelFingerprint == "" || record.Token == "" {
		return endpointRecord{}, errors.New("invalid session endpoint record")
	}
	return record, nil
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	return errA == nil && errB == nil && filepath.Clean(aa) == filepath.Clean(bb)
}
