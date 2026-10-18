package orcabox

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestSupplierErrorsAreRedactedWithoutLosingUsageOrNormalContent(t *testing.T) {
	secret := "SUPPLIER_CREDENTIAL_FIXTURE"
	raw := `data: {"type":"response.output_text.delta","delta":"normal answer"}` + "\n\n" +
		`data: {"type":"response.failed","response":{"id":"resp-fixture","usage":{"input_tokens":20,"output_tokens":10},"error":{"message":"` + secret + `","code":"` + secret + `","param":"` + secret + `","metadata":{"original":"` + secret + `"}}}}` + "\n\n"
	capture := &usageCapture{stream: true}
	body := sanitizeSupplierBody(&observedBody{ReadCloser: io.NopCloser(strings.NewReader(raw)), c: capture}, true)
	result, e := io.ReadAll(body)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(result, []byte(secret)) || !bytes.Contains(result, []byte("normal answer")) || !bytes.Contains(result, []byte("response.failed")) {
		t.Fatal("error secret leaked or normal SSE altered")
	}
	if capture.usage == nil || capture.usage.Input != 20 || capture.usage.Output != 10 {
		t.Fatal("failed response usage lost during redaction")
	}
	jsonBody := sanitizeSupplierBody(io.NopCloser(strings.NewReader(`{"error":{"message":"`+secret+`"}}`)), false)
	result, e = io.ReadAll(jsonBody)
	if e != nil || bytes.Contains(result, []byte(secret)) {
		t.Fatal("JSON supplier error leaked")
	}
}

func TestSupplierFrameSizeIsBounded(t *testing.T) {
	for _, stream := range []bool{true, false} {
		body := sanitizeSupplierBody(io.NopCloser(strings.NewReader(strings.Repeat("x", maxSupplierFrame+1))), stream)
		if _, e := io.ReadAll(body); e == nil {
			t.Fatal("oversized supplier frame accepted")
		}
	}
}
