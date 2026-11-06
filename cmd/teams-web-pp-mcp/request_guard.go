package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// guardRequest runs before MCP request dispatch. JSON-RPC and tool envelope
// member names must be exact, and duplicate object members are rejected to
// prevent a policy filter and the server from interpreting a request differently.
// It does not inspect or log values. Notifications are handled separately by
// the upstream stdio server and are outside this request-dispatch guard.
func guardRequest(_ context.Context, _ any, message any) error {
	raw, ok := message.(json.RawMessage)
	if !ok {
		return errors.New("invalid JSON-RPC request")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := readGuardValue(decoder, guardEnvelope, 0); err != nil {
		return errors.New("invalid or ambiguous JSON-RPC request")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("invalid JSON-RPC request")
	}
	return nil
}

type guardScope int

const (
	guardOther guardScope = iota
	guardEnvelope
	guardParams
)

var envelopeMembers = []string{"jsonrpc", "id", "method", "params", "result", "error"}
var paramsMembers = []string{"name", "arguments"}

func readGuardValue(decoder *json.Decoder, scope guardScope, depth int) error {
	if depth > 128 {
		return errors.New("JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if scope == guardEnvelope && (!isDelim || delim != '{') {
		return errors.New("JSON-RPC envelope must be an object")
	}
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			member, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := member.(string)
			if !ok {
				return errors.New("non-string JSON object member")
			}
			if _, duplicate := seen[name]; duplicate {
				return errors.New("duplicate JSON object member")
			}
			seen[name] = struct{}{}
			var reserved []string
			switch scope {
			case guardEnvelope:
				reserved = envelopeMembers
			case guardParams:
				reserved = paramsMembers
			}
			for _, expected := range reserved {
				if name != expected && strings.EqualFold(name, expected) {
					return errors.New("incorrectly cased JSON-RPC member")
				}
			}
			childScope := guardOther
			if scope == guardEnvelope && name == "params" {
				childScope = guardParams
			}
			if err := readGuardValue(decoder, childScope, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := readGuardValue(decoder, guardOther, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}
