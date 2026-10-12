package workloadagent

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
)

const maxSigningKeyFileBytes = 16 << 10

// LoadTicketSigningKey parses one PKCS#8 Ed25519 private key from a bounded
// mounted file. Callers must not source private signing material from
// repository data, queue messages, or environment variables.
func LoadTicketSigningKey(reader io.Reader) (ed25519.PrivateKey, error) {
	if reader == nil {
		return nil, ErrAgentTicket
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxSigningKeyFileBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxSigningKeyFileBytes {
		return nil, ErrAgentTicket
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(rest) != 0 {
		return nil, ErrAgentTicket
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, ErrAgentTicket
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(key) != ed25519.PrivateKeySize {
		return nil, ErrAgentTicket
	}
	return append(ed25519.PrivateKey(nil), key...), nil
}

// LoadTicketSigningKeyFile opens a mounted signing-key file. Errors are
// intentionally collapsed so filesystem details or key bytes are not exposed
// in logs by callers.
func LoadTicketSigningKeyFile(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("Agent ticket signing key file is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open Agent ticket signing key file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("Agent ticket signing key file must be regular and private")
	}
	key, err := LoadTicketSigningKey(file)
	if err != nil {
		return nil, errors.New("load Agent ticket signing key")
	}
	return key, nil
}
