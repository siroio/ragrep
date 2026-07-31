package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"

	"github.com/siroio/ragrep/internal/embed"
)

func requestSession(endpointPath, dbPath string, request sessionRequest) (sessionResponse, error) {
	record, err := readEndpointRecord(endpointPath)
	if err != nil {
		return sessionResponse{}, fmt.Errorf("invalid session endpoint %s: %w", endpointPath, err)
	}
	absDB, err := filepath.Abs(dbPath)
	if err != nil {
		return sessionResponse{}, err
	}
	if !samePath(record.DBPath, absDB) {
		return sessionResponse{}, fmt.Errorf("session endpoint database does not match --db")
	}
	dir, err := embed.CacheDir()
	if err != nil {
		return sessionResponse{}, err
	}
	fingerprint, err := embed.Fingerprint(dir)
	if err != nil {
		return sessionResponse{}, err
	}
	if record.ModelFingerprint != fingerprint {
		return sessionResponse{}, fmt.Errorf("session endpoint model fingerprint does not match this client")
	}
	request.Token = record.Token
	request.DBPath = absDB
	request.ModelFingerprint = fingerprint
	conn, err := net.Dial("tcp", record.Address)
	if err != nil {
		return sessionResponse{}, fmt.Errorf("stale session endpoint %s: %w", endpointPath, err)
	}
	defer conn.Close()
	data, err := json.Marshal(request)
	if err != nil {
		return sessionResponse{}, err
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return sessionResponse{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return sessionResponse{}, err
	}
	var response sessionResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return sessionResponse{}, fmt.Errorf("invalid session response: %w", err)
	}
	if response.Error != "" {
		return sessionResponse{}, fmt.Errorf("session: %s", response.Error)
	}
	return response, nil
}

func cmdSearchSession(endpoint, db, mode, query string, k int, tags []string, asJSON bool) int {
	response, err := requestSession(endpoint, db, sessionRequest{Operation: "search", Mode: mode, Query: query, K: k, Tags: tags})
	if err != nil {
		return fail(err)
	}
	for i := range response.Hits {
		if i < len(response.Mtimes) {
			response.Hits[i].Mtime = response.Mtimes[i]
		}
	}
	return printSearchResults(response.Hits, db, asJSON)
}

func cmdGetSession(endpoint, db, arg, lines string, para, context int) int {
	wsRoot, err := workspaceRoot(db)
	if err != nil {
		return fail(err)
	}
	key := path.Clean(filepath.ToSlash(arg))
	response, err := requestSession(endpoint, db, sessionRequest{Operation: "get", Path: key, Lines: lines, Para: para, Context: context})
	if err != nil {
		return fail(err)
	}
	if response.NotFound {
		if normKey, nerr := normPath(arg, wsRoot); nerr == nil {
			response, err = requestSession(endpoint, db, sessionRequest{Operation: "get", Path: normKey, Lines: lines, Para: para, Context: context})
			if err != nil {
				return fail(err)
			}
		}
	}
	if response.NotFound {
		fmt.Fprintln(os.Stderr, "not found")
		return 2
	}
	fmt.Println(response.Content)
	return 0
}
