package orcabox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const maxSupplierFrame = 2 << 20

// Usage is captured from the original bytes first. Only protocol error fields
// are replaced; ordinary model content, tool arguments and SSE timing survive.
type safeSupplierBody struct {
	io.ReadCloser
	scanner *bufio.Scanner
	pending []byte
	loaded  bool
}

func sanitizeSupplierBody(body io.ReadCloser, stream bool) io.ReadCloser {
	s := &safeSupplierBody{ReadCloser: body}
	if stream {
		s.scanner = bufio.NewScanner(body)
		s.scanner.Buffer(make([]byte, 4096), maxSupplierFrame)
	}
	return s
}

func (s *safeSupplierBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(s.pending) == 0 {
		if s.scanner != nil {
			if !s.scanner.Scan() {
				if e := s.scanner.Err(); e != nil {
					return 0, errors.New("supplier stream frame unavailable")
				}
				return 0, io.EOF
			}
			line := s.scanner.Bytes()
			if bytes.HasPrefix(line, []byte("data:")) {
				line = append([]byte("data: "), safeProtocolError(bytes.TrimSpace(line[5:]))...)
			}
			s.pending = append(append([]byte(nil), line...), '\n')
		} else {
			if s.loaded {
				return 0, io.EOF
			}
			s.loaded = true
			raw, e := io.ReadAll(io.LimitReader(s.ReadCloser, maxSupplierFrame+1))
			if e != nil || len(raw) > maxSupplierFrame {
				return 0, errors.New("supplier response unavailable")
			}
			s.pending = safeProtocolError(raw)
			if len(s.pending) == 0 {
				return 0, io.EOF
			}
		}
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func safeProtocolError(raw []byte) []byte {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return raw
	}
	var kind string
	json.Unmarshal(body["type"], &kind)
	if kind == "error" {
		result, _ := json.Marshal(map[string]string{"type": "error", "code": "orcabox_supplier_error", "message": "模型服务暂不可用，请稍后重试。"})
		return result
	}
	changed := false
	if value, ok := body["error"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		body["error"] = json.RawMessage(`{"type":"server_error","code":"orcabox_supplier_error","message":"模型服务暂不可用，请稍后重试。"}`)
		changed = true
	}
	if response, ok := body["response"]; ok {
		clean := safeProtocolError(response)
		if !bytes.Equal(clean, response) {
			body["response"] = clean
			changed = true
		}
	}
	if !changed {
		return raw
	}
	clean, e := json.Marshal(body)
	if e != nil {
		return raw
	}
	return clean
}
