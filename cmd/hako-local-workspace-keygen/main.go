//go:build linux

// hako-local-workspace-keygen creates private local API/Gateway/Guest keys and
// a self-signed localhost certificate for a single-host Cloud Hypervisor setup.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func main() {
	directory := flag.String("dir", "", "new private directory for local Workspace keys")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: hako-local-workspace-keygen -dir /absolute/private/path")
		os.Exit(2)
	}
	if err := generate(*directory, time.Now().UTC()); err != nil {
		fmt.Fprintln(os.Stderr, "create local Workspace keys:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "Created private local Workspace keys and localhost certificate.")
}

func generate(directory string, now time.Time) (returnErr error) {
	directory = strings.TrimSpace(directory)
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == "/" || now.IsZero() {
		return errors.New("key directory must be a clean absolute path and time must be set")
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return errors.New("key directory must not already exist")
	}
	defer func() {
		if returnErr != nil {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Chmod(directory, 0700); err != nil {
		return errors.New("set local Workspace key directory permissions")
	}
	info, err := os.Lstat(directory)
	stat, ok := func() (*syscall.Stat_t, bool) {
		if info == nil {
			return nil, false
		}
		value, valid := info.Sys().(*syscall.Stat_t)
		return value, valid
	}()
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("local Workspace key directory must be private and owned by this user")
	}
	sessionPublic, sessionPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("generate Workspace Session key")
	}
	agentPublic, agentPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("generate Workspace Agent ticket key")
	}
	if !keyIDPattern.MatchString("local_session") || !keyIDPattern.MatchString("local_agent") {
		return errors.New("local key IDs are invalid")
	}
	sessionPEM, err := encodePrivateKey(sessionPrivate)
	if err != nil {
		return err
	}
	if err := writePrivate(directory, "session-private.pem", sessionPEM); err != nil {
		return err
	}
	sessionPublicJSON, err := jsonKey("local_session", base64.RawStdEncoding.EncodeToString(sessionPublic))
	if err != nil {
		return errors.New("encode Workspace Session public key")
	}
	if err := writePrivate(directory, "session-public.json", sessionPublicJSON); err != nil {
		return err
	}
	hmacKey, err := randomKey()
	if err != nil {
		return errors.New("generate local Workspace HMAC key")
	}
	if err := writePrivate(directory, "session-hmac.key", hmacKey); err != nil {
		return err
	}
	agentPEM, err := encodePrivateKey(agentPrivate)
	if err != nil {
		return err
	}
	if err := writePrivate(directory, "agent-private.pem", agentPEM); err != nil {
		return err
	}
	agentPublicJSON, err := jsonKey("local_agent", base64.RawURLEncoding.EncodeToString(agentPublic))
	if err != nil {
		return errors.New("encode Workspace Agent public key")
	}
	if err := writePrivate(directory, "agent-public.json", agentPublicJSON); err != nil {
		return err
	}
	certPEM, keyPEM, err := localhostCertificate(now)
	if err != nil {
		return err
	}
	if err := writePrivate(directory, "gateway-cert.pem", certPEM); err != nil {
		return err
	}
	if err := writePrivate(directory, "gateway-key.pem", keyPEM); err != nil {
		return err
	}
	return nil
}

func encodePrivateKey(key ed25519.PrivateKey) ([]byte, error) {
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, errors.New("marshal generated Ed25519 key")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), nil
}

func randomKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		clear(key)
		return nil, err
	}
	return key, nil
}

func jsonKey(keyID, publicKey string) ([]byte, error) {
	encoded, err := json.Marshal(map[string]string{keyID: publicKey})
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func localhostCertificate(now time.Time) ([]byte, []byte, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, errors.New("generate localhost TLS key")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, errors.New("generate localhost TLS certificate serial")
	}
	certificate := x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &certificate, &certificate, publicKey, privateKey)
	if err != nil {
		return nil, nil, errors.New("create localhost TLS certificate")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, errors.New("encode localhost TLS key")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func writePrivate(directory, name string, data []byte) error {
	path := filepath.Join(directory, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("create a local Workspace key file without replacement")
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	clear(data)
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("write local Workspace key file")
	}
	return nil
}
