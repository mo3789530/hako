package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBootstrapPublicKey(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runner.pub")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadBootstrapPublicKey(path)
	if err != nil || !got.Equal(publicKey) {
		t.Fatalf("loadBootstrapPublicKey() mismatch: err=%v", err)
	}
	clear(got)
}

func TestLoadBootstrapPublicKeyRejectsPrivateOrMalformedMaterial(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runner.pem")
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBootstrapPublicKey(path); err == nil {
		t.Fatal("private signing key was accepted in guest image")
	}
	if _, err := loadBootstrapPublicKey("relative.pub"); err == nil {
		t.Fatal("relative key path accepted")
	}
}

func TestLoadBootstrapPublicKeyRejectsInvalidFiles(t *testing.T) {
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongDER, err := x509.MarshalPKIXPublicKey(&wrongKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(edKey)
	if err != nil {
		t.Fatal(err)
	}
	badDER := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("not DER")})
	validDER, err := x509.MarshalPKIXPublicKey(edKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
		path string
	}{
		{name: "missing file", path: filepath.Join(t.TempDir(), "missing.pub")},
		{name: "empty file", data: nil},
		{name: "oversized file", data: []byte(strings.Repeat("x", 4097))},
		{name: "malformed PEM", data: []byte("not pem")},
		{name: "private key", data: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})},
		{name: "wrong key type", data: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: wrongDER})},
		{name: "malformed DER", data: badDER},
		{name: "trailing data", data: append(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: validDER}), []byte("unexpected")...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := test.path
			if path == "" {
				path = filepath.Join(t.TempDir(), "bootstrap.pub")
				if err := os.WriteFile(path, test.data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadBootstrapPublicKey(path); err == nil {
				t.Fatal("invalid bootstrap key file was accepted")
			}
		})
	}
	for _, path := range []string{"relative.pub", " /absolute/key.pub"} {
		if _, err := loadBootstrapPublicKey(path); err == nil {
			t.Errorf("invalid bootstrap key path %q was accepted", path)
		}
	}
}

func TestServeGuestValidatesDependenciesAndShutsDownOnCancellation(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	for _, test := range []struct {
		name     string
		ctx      context.Context
		listener net.Listener
		server   *http.Server
	}{
		{name: "nil context", listener: listener, server: server},
		{name: "nil listener", ctx: context.Background(), server: server},
		{name: "nil server", ctx: context.Background(), listener: listener},
		{name: "nil handler", ctx: context.Background(), listener: listener, server: &http.Server{}},
	} {
		if err := serveGuest(test.ctx, test.listener, test.server); err == nil {
			t.Errorf("serveGuest accepted %s", test.name)
		}
	}
	_ = listener.Close()

	activeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := serveGuest(ctx, activeListener, &http.Server{Handler: handler}); err != nil {
		t.Fatalf("serveGuest cancellation shutdown: %v", err)
	}
}

func TestServeGuestReportsUnexpectedListenerExit(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	err = serveGuest(context.Background(), listener, server)
	if err == nil || !strings.Contains(err.Error(), "stopped unexpectedly") || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serveGuest unexpected-exit error = %v", err)
	}
}

func TestDefaultString(t *testing.T) {
	if got := defaultString("value", "fallback"); got != "value" {
		t.Fatalf("defaultString(value) = %q", got)
	}
	if got := defaultString("", "fallback"); got != "fallback" {
		t.Fatalf("defaultString(empty) = %q", got)
	}
}
