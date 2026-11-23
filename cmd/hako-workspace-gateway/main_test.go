package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDeploymentConfigRequiresCompleteSecureConfiguration(t *testing.T) {
	values := map[string]string{
		"HAKO_RESOURCE_PLANE_ID": "rp_a", "HAKO_GATEWAY_TLS_CERT_FILE": "/tls/cert.pem",
		"HAKO_GATEWAY_TLS_KEY_FILE": "/tls/key.pem", "HAKO_GATEWAY_SESSION_PUBLIC_KEYS_FILE": "/keys/session.json",
		"HAKO_GATEWAY_SESSION_LOOKUP_ENDPOINT": "https://api.hako.test", "HAKO_GATEWAY_SESSION_LOOKUP_KEY_FILE": "/keys/lookup",
		"HAKO_GATEWAY_CONNECTION_LEASE_ENDPOINT": "https://api.hako.test", "HAKO_GATEWAY_CONNECTION_LEASE_KEY_FILE": "/keys/lease",
		"HAKO_GATEWAY_AGENT_TICKET_KEY_ID": "agent-v1", "HAKO_GATEWAY_AGENT_TICKET_KEY_FILE": "/keys/agent.pem",
		"HAKO_GATEWAY_PUBLIC_ORIGIN": "https://gateway.hako.test", "HAKO_GATEWAY_ALLOWED_ORIGINS": "https://app.hako.test, https://ide.hako.test",
		"HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET": "hako-workspace-journal", "AWS_REGION": "ap-northeast-1",
	}
	getenv := func(name string) string { return values[name] }
	cfg, err := loadDeploymentConfig(getenv)
	if err != nil {
		t.Fatalf("load valid configuration: %v", err)
	}
	if cfg.ListenAddress != ":8443" || cfg.MaxConnections != 500 || cfg.MaxConnectionsPerWS != 10 ||
		cfg.PublicOrigin != "https://gateway.hako.test" || cfg.RuntimeImplementation != "lambda-microvm" ||
		len(cfg.AllowedOrigins) != 2 || cfg.SessionRecheckInterval.String() != "15s" {
		t.Fatalf("unexpected defaults/config: %+v", cfg)
	}

	localValues := make(map[string]string, len(values)+3)
	for key, value := range values {
		localValues[key] = value
	}
	delete(localValues, "HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET")
	delete(localValues, "AWS_REGION")
	localValues["HAKO_WORKSPACE_RUNTIME_IMPLEMENTATION"] = "firecracker-local"
	localValues["HAKO_LOCAL_FIRECRACKER_STATE_DIR"] = "/var/lib/hako/firecracker"
	localValues["HAKO_LOCAL_WORKSPACE_JOURNAL_DIR"] = "/var/lib/hako/workspace-journal"
	localCfg, err := loadDeploymentConfig(func(key string) string { return localValues[key] })
	if err != nil || localCfg.RuntimeImplementation != "firecracker-local" || localCfg.JournalBucket != "" {
		t.Fatalf("load AWS-free local Gateway configuration: cfg=%+v err=%v", localCfg, err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"missing local state":         func(values map[string]string) { delete(values, "HAKO_LOCAL_FIRECRACKER_STATE_DIR") },
		"S3 journal on local runtime": func(values map[string]string) { values["HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET"] = "unexpected" },
		"unknown runtime":             func(values map[string]string) { values["HAKO_WORKSPACE_RUNTIME_IMPLEMENTATION"] = "unknown" },
	} {
		t.Run("local/"+name, func(t *testing.T) {
			mutated := make(map[string]string, len(localValues))
			for key, value := range localValues {
				mutated[key] = value
			}
			mutate(mutated)
			if _, err := loadDeploymentConfig(func(key string) string { return mutated[key] }); err == nil {
				t.Fatal("invalid local Gateway configuration was accepted")
			}
		})
	}

	for name, value := range map[string]string{
		"missing resource plane": "", "plain-text origin": "http://gateway.hako.test", "origin with path": "https://gateway.hako.test/path",
	} {
		t.Run(name, func(t *testing.T) {
			mutated := make(map[string]string, len(values))
			for key, value := range values {
				mutated[key] = value
			}
			if name == "missing resource plane" {
				mutated["HAKO_RESOURCE_PLANE_ID"] = value
			} else {
				mutated["HAKO_GATEWAY_PUBLIC_ORIGIN"] = value
			}
			if _, err := loadDeploymentConfig(func(key string) string { return mutated[key] }); err == nil {
				t.Fatal("expected invalid deployment configuration to be rejected")
			}
		})
	}
	values["HAKO_GATEWAY_MAX_CONNECTIONS"] = "not-a-number"
	if _, err := loadDeploymentConfig(getenv); err == nil {
		t.Fatal("expected invalid connection limit to be rejected")
	}
	for _, value := range []string{"0s", "500ms", "61s", "not-a-duration"} {
		values["HAKO_GATEWAY_MAX_CONNECTIONS"] = "500"
		values["HAKO_GATEWAY_SESSION_RECHECK_INTERVAL"] = value
		if _, err := loadDeploymentConfig(getenv); err == nil {
			t.Errorf("expected invalid Session recheck interval %q to be rejected", value)
		}
	}
}

func TestLoadSessionPublicKeysAndRejectSecretFileSymlink(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "session-keys.json")
	data, _ := json.Marshal(map[string]string{"session-v1": base64.RawStdEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey))})
	if err := os.WriteFile(keyPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := loadSessionPublicKeys(keyPath)
	if err != nil || len(keys["session-v1"]) != ed25519.PublicKeySize {
		t.Fatalf("load public keys: keys=%v err=%v", keys, err)
	}

	secretPath := filepath.Join(dir, "agent.pem")
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	secretPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	if err := os.WriteFile(secretPath, secretPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := readBoundedFile(secretPath, maxGatewayKeyFileBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseAgentPrivateKey(read)
	wipe(read)
	if err != nil || !parsed.Equal(privateKey) {
		t.Fatalf("parse private key: err=%v", err)
	}
	if err := os.Chmod(secretPath, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(secretPath, maxGatewayKeyFileBytes, true); err == nil {
		t.Fatal("expected writable secret key file to be rejected")
	}
	symlinkPath := filepath.Join(dir, "secret-link")
	if err := os.Symlink(secretPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(symlinkPath, maxGatewayKeyFileBytes, true); err == nil {
		t.Fatal("expected secret key symlink to be rejected")
	}
}

func TestSessionPublicKeyResolverReloadsRotatedOverlapSet(t *testing.T) {
	oldPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session-keys.json")
	writeKeys := func(keys map[string]ed25519.PublicKey) {
		t.Helper()
		encoded := make(map[string]string, len(keys))
		for id, key := range keys {
			encoded[id] = base64.RawStdEncoding.EncodeToString(key)
		}
		data, err := json.Marshal(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resolver := sessionPublicKeyResolver(path)
	writeKeys(map[string]ed25519.PublicKey{"session-old": oldPublic})
	first, err := resolver()
	if err != nil || len(first) != 1 {
		t.Fatalf("initial key set = %v, %v", first, err)
	}
	writeKeys(map[string]ed25519.PublicKey{"session-old": oldPublic, "session-new": newPublic})
	overlap, err := resolver()
	if err != nil || len(overlap) != 2 || len(overlap["session-new"]) != ed25519.PublicKeySize {
		t.Fatalf("overlap key set was not reloaded: %v, %v", overlap, err)
	}
	writeKeys(map[string]ed25519.PublicKey{"session-new": newPublic})
	retired, err := resolver()
	if err != nil || len(retired) != 1 || retired["session-old"] != nil {
		t.Fatalf("retired key remains active: %v, %v", retired, err)
	}
	if err := os.WriteFile(path, []byte(`{"bad key":"not-a-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver(); err == nil {
		t.Fatal("invalid key rotation was accepted")
	}
}
