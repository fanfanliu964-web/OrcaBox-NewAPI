package orcabox

import (
	"bytes"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"io"
	"strings"
)

// Observe the supplier body before New API can synthesize missing token usage.
// Nothing except numeric usage is retained after a frame is parsed.
type usageCapture struct {
	onUsage            func(*Usage)
	pending            []byte
	usage              *Usage
	stream             bool
	oversize           bool
	supplierRequestID  string
	supplierResponseID string
	opaqueDigests      map[string]struct{}
}

func (c *usageCapture) feed(b []byte) {
	if c.oversize {
		return
	}
	c.pending = append(c.pending, b...)
	if c.stream {
		for {
			i := bytes.IndexByte(c.pending, '\n')
			if i < 0 {
				break
			}
			line := c.pending[:i]
			if bytes.HasPrefix(line, []byte("data: ")) {
				c.parse(bytes.TrimSpace(line[6:]))
			}
			c.pending = append([]byte(nil), c.pending[i+1:]...)
		}
	}
	if len(c.pending) > 2<<20 {
		c.pending = nil
		c.oversize = true
	}
}
func (c *usageCapture) parse(b []byte) {
	var v map[string]json.RawMessage
	if json.Unmarshal(b, &v) != nil {
		return
	}
	var responseID string
	_ = json.Unmarshal(v["id"], &responseID)
	if clean := safeSupplierID(responseID); clean != "" {
		c.supplierResponseID = clean
	}
	var eventType string
	_ = json.Unmarshal(v["type"], &eventType)
	if eventType == "response.output_item.done" {
		var item map[string]any
		if json.Unmarshal(v["item"], &item) == nil {
			c.recordOpaqueItem(item)
		}
	}
	var outputItems []map[string]any
	if json.Unmarshal(v["output"], &outputItems) == nil {
		for _, item := range outputItems {
			c.recordOpaqueItem(item)
		}
	}
	if eventType != "" && eventType != "response.completed" && eventType != "response.incomplete" && eventType != "response.failed" {
		return
	}
	raw := v["usage"]
	if len(raw) == 0 {
		if r := v["response"]; len(r) > 0 {
			c.parse(r)
		}
		return
	}
	var u map[string]json.RawMessage
	if json.Unmarshal(raw, &u) != nil {
		return
	}
	read := func(names ...string) (int64, bool) {
		for _, n := range names {
			if r, ok := u[n]; ok {
				var x int64
				if json.Unmarshal(r, &x) == nil {
					return x, true
				}
			}
		}
		return 0, false
	}
	input, ok1 := read("input_tokens", "prompt_tokens")
	output, ok2 := read("output_tokens", "completion_tokens")
	if !ok1 || !ok2 {
		return
	}
	parsed := Usage{Input: input, Output: output}
	for _, n := range []string{"input_tokens_details", "prompt_tokens_details"} {
		var d struct {
			Cached int64 `json:"cached_tokens"`
			Write  int64 `json:"cache_creation_tokens"`
		}
		if json.Unmarshal(u[n], &d) == nil {
			parsed.Cached = d.Cached
			parsed.CacheWrite = d.Write
			break
		}
	}
	if n, ok := read("cache_creation_input_tokens"); ok {
		parsed.CacheWrite = n
	}
	c.usage = &parsed
	if c.onUsage != nil {
		c.onUsage(&parsed)
	}
}

func (c *usageCapture) recordOpaqueItem(item map[string]any) {
	if item["type"] != "reasoning" {
		return
	}
	if encrypted, ok := item["encrypted_content"].(string); !ok || encrypted == "" {
		return
	}
	if c.opaqueDigests == nil {
		c.opaqueDigests = map[string]struct{}{}
	}
	c.opaqueDigests[opaqueDigest(item)] = struct{}{}
}
func (c *usageCapture) metadata() SettlementMetadata {
	m := SettlementMetadata{RequestID: c.supplierRequestID, ResponseID: c.supplierResponseID}
	for digest := range c.opaqueDigests {
		m.OpaqueDigests = append(m.OpaqueDigests, digest)
	}
	return m
}

type observedBody struct {
	io.ReadCloser
	c *usageCapture
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	b.c.feed(p[:n])
	if e == io.EOF && !b.c.stream {
		b.c.parse(b.c.pending)
		b.c.pending = nil
	}
	return n, e
}
func ObserveSupplier(c *gin.Context, body io.ReadCloser, contentType string, requestIDs ...string) io.ReadCloser {
	if !c.GetBool(Managed) {
		return body
	}
	v, ok := c.Get("orcabox_capture")
	if !ok {
		return body
	}
	capture := v.(*usageCapture)
	for _, id := range requestIDs {
		if clean := safeSupplierID(id); clean != "" {
			capture.supplierRequestID = clean
			break
		}
	}
	capture.stream = bytes.Contains([]byte(contentType), []byte("text/event-stream"))
	return sanitizeSupplierBody(&observedBody{ReadCloser: body, c: capture}, capture.stream)
}

func safeSupplierID(id string) string {
	if len(id) < 4 || len(id) > 160 || strings.Contains(id, "sk-") || strings.Contains(id, "sk_") || strings.Contains(id, "rk_") || strings.Contains(id, "whsec_") {
		return ""
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return ""
		}
	}
	return id
}
