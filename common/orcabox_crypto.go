package common

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"strings"
)

const channelEnvelope = "enc.ob.v1:"

func channelAEAD() (cipher.AEAD, error) {
	key, e := hex.DecodeString(os.Getenv("ORCABOX_UPSTREAM_ENCRYPTION_KEY"))
	if e != nil || len(key) != 32 {
		return nil, errors.New("channel encryption unavailable")
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(block)
}
func EncryptOrcaboxChannelKey(key string) (string, error) {
	if os.Getenv("ORCABOX_MANAGED_GATEWAY") != "true" || key == "" || strings.HasPrefix(key, channelEnvelope) {
		return key, nil
	}
	a, e := channelAEAD()
	if e != nil {
		return "", e
	}
	nonce := make([]byte, a.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return "", e
	}
	sealed := a.Seal(nonce, nonce, []byte(key), []byte("orcabox:channel"))
	return channelEnvelope + base64.RawStdEncoding.EncodeToString(sealed), nil
}
func DecryptOrcaboxChannelKey(key string) (string, error) {
	if !strings.HasPrefix(key, channelEnvelope) {
		if os.Getenv("ORCABOX_MANAGED_GATEWAY") == "true" {
			return "", errors.New("unencrypted managed channel")
		}
		return key, nil
	}
	a, e := channelAEAD()
	if e != nil {
		return "", e
	}
	b, e := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(key, channelEnvelope))
	if e != nil || len(b) < a.NonceSize() {
		return "", errors.New("invalid channel envelope")
	}
	raw, e := a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], []byte("orcabox:channel"))
	if e != nil {
		return "", errors.New("channel decryption failed")
	}
	return string(raw), nil
}
