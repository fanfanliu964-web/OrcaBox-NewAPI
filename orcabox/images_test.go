package orcabox

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompressedImageDecodingIsBoundedAcrossAccounts(t *testing.T) {
	imageDecodeSlots <- struct{}{}
	defer func() { <-imageDecodeSlots }()
	url := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="
	if e := validateImage(url); !errors.Is(e, errImageDecodeBusy) {
		t.Fatal("pixel decoding exceeded global memory admission", e)
	}
}

func TestVerifiedImageModelsNormalizeDetailAndSupportFourImages(t *testing.T) {
	url := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="
	images := []any{}
	for i := 0; i < 4; i++ {
		images = append(images, map[string]any{"type": "input_image", "image_url": url, "detail": "original"})
	}
	for _, model := range []string{"gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6-astra"} {
		t.Run(model, func(t *testing.T) {
			b := map[string]any{"model": model, "input": []any{map[string]any{"role": "user", "content": images}}}
			bound, _, err := validateRequest(b, "/v1/responses")
			require.NoError(t, err)
			assert.GreaterOrEqual(t, bound, int64(4*24576))
			assert.LessOrEqual(t, bound+8192, int64(128000))
		})
	}
	for _, image := range images {
		if image.(map[string]any)["detail"] != "high" {
			t.Fatal("unbounded original detail forwarded")
		}
	}
	unknown := map[string]any{"model": "unpublished", "input": []any{map[string]any{"type": "input_image", "image_url": url}}}
	if _, _, e := validateRequest(unknown, "/v1/responses"); e == nil {
		t.Fatal("unknown image pricing accepted")
	}
	// Tool schema property names describe text; they are not image attachments.
	schema := map[string]any{"tools": []any{map[string]any{"type": "function", "parameters": map[string]any{"type": "object", "properties": map[string]any{"image_url": map[string]any{"type": "string"}}}}}}
	if _, _, e := validateRequest(schema, "/v1/responses"); e != nil {
		t.Fatal("tool schema mistaken for uploaded image", e)
	}
}
