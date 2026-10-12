package workloadagent

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"regexp"
)

const maxTrustedKeysFileBytes = 16 << 10

var trustedKeyEncoding = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// LoadTrustedKeys reads only public Ed25519 verification keys from an
// image-owned file. Private signing material must stay in the Resource Plane
// Controller and must never be copied into the MicroVM image.
func LoadTrustedKeys(reader io.Reader) (map[string]ed25519.PublicKey, error) {
	if reader == nil {
		return nil, ErrAgentTicket
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxTrustedKeysFileBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxTrustedKeysFileBytes {
		return nil, ErrAgentTicket
	}
	var encoded map[string]string
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&encoded) != nil || decoder.Decode(new(any)) != io.EOF || len(encoded) == 0 || len(encoded) > 32 {
		return nil, ErrAgentTicket
	}
	keys := make(map[string]ed25519.PublicKey, len(encoded))
	for id, value := range encoded {
		if !keyIDPattern.MatchString(id) || !trustedKeyEncoding.MatchString(value) {
			return nil, ErrAgentTicket
		}
		key, err := base64.RawURLEncoding.DecodeString(value)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, ErrAgentTicket
		}
		keys[id] = append(ed25519.PublicKey(nil), key...)
	}
	return keys, nil
}
