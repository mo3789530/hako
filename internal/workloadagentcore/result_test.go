package workloadagentcore

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func testProposalArchive(t *testing.T, path, content string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	file, err := writer.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestDecodeInvocationResponseValidatesAttemptAndProposalDigest(t *testing.T) {
	_, expected, _ := preparedAgentCoreInput(t)
	proposal := testProposalArchive(t, "app.go", "package app\n")
	digest := sha256.Sum256(proposal)
	response := runtimeInvocationResponse{SchemaVersion: 1, Attempt: attemptPins(expected), DraftStatus: "proposal_ready",
		Iterations: 2, TreeDigest: strings.Repeat("a", 64), Proposal: &runtimeProposalResponse{
			SHA256: hex.EncodeToString(digest[:]), DataBase64: base64.StdEncoding.EncodeToString(proposal)}}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeInvocationResponse(body, expected)
	if err != nil || got.Attempt != attemptPins(expected) || got.Status != "proposal_ready" || got.Iterations != 2 ||
		got.TreeDigest != response.TreeDigest || got.ProposalSHA != response.Proposal.SHA256 || string(got.ProposalData) != string(proposal) {
		t.Fatalf("decoded proposal = %+v, err=%v", got, err)
	}
}

func TestDecodeInvocationResponseRejectsMalformedOrUntrustedOutput(t *testing.T) {
	_, expected, _ := preparedAgentCoreInput(t)
	proposal := testProposalArchive(t, "app.go", "package app\n")
	digest := sha256.Sum256(proposal)
	base := runtimeInvocationResponse{SchemaVersion: 1, Attempt: attemptPins(expected), DraftStatus: "proposal_ready",
		Iterations: 1, TreeDigest: strings.Repeat("b", 64), Proposal: &runtimeProposalResponse{
			SHA256: hex.EncodeToString(digest[:]), DataBase64: base64.StdEncoding.EncodeToString(proposal)}}
	valid, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"duplicate key":   []byte(strings.Replace(string(valid), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1)),
		"unknown field":   []byte(strings.TrimSuffix(string(valid), "}") + `,"verified":true}`),
		"foreign attempt": func() []byte { v := base; v.Attempt.TenantID = "foreign"; b, _ := json.Marshal(v); return b }(),
		"bad proposal digest": func() []byte {
			v := base
			p := *v.Proposal
			p.SHA256 = strings.Repeat("c", 64)
			v.Proposal = &p
			b, _ := json.Marshal(v)
			return b
		}(),
		"bad base64": func() []byte {
			v := base
			p := *v.Proposal
			p.DataBase64 = "%%%"
			v.Proposal = &p
			b, _ := json.Marshal(v)
			return b
		}(),
		"malformed proposal archive": func() []byte {
			v := base
			p := *v.Proposal
			badArchive := []byte("not a ZIP archive")
			d := sha256.Sum256(badArchive)
			p.SHA256, p.DataBase64 = hex.EncodeToString(d[:]), base64.StdEncoding.EncodeToString(badArchive)
			v.Proposal = &p
			b, _ := json.Marshal(v)
			return b
		}(),
		"protected proposal path": func() []byte {
			v := base
			p := *v.Proposal
			badArchive := testProposalArchive(t, "infra/main.tf", "terraform {}\n")
			d := sha256.Sum256(badArchive)
			p.SHA256, p.DataBase64 = hex.EncodeToString(d[:]), base64.StdEncoding.EncodeToString(badArchive)
			v.Proposal = &p
			b, _ := json.Marshal(v)
			return b
		}(),
		"credential marker": func() []byte {
			v := base
			p := *v.Proposal
			badArchive := testProposalArchive(t, "app.go", "-----BEGIN PRIVATE KEY-----\n")
			d := sha256.Sum256(badArchive)
			p.SHA256, p.DataBase64 = hex.EncodeToString(d[:]), base64.StdEncoding.EncodeToString(badArchive)
			v.Proposal = &p
			b, _ := json.Marshal(v)
			return b
		}(),
		"proposal missing":       func() []byte { v := base; v.Proposal = nil; b, _ := json.Marshal(v); return b }(),
		"no proposal with bytes": func() []byte { v := base; v.DraftStatus = "no_proposal"; b, _ := json.Marshal(v); return b }(),
		"unknown status":         func() []byte { v := base; v.DraftStatus = "verified"; b, _ := json.Marshal(v); return b }(),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeInvocationResponse(body, expected); err == nil {
				t.Fatal("accepted untrusted response")
			}
		})
	}
	noProposal := runtimeInvocationResponse{SchemaVersion: 1, Attempt: attemptPins(expected), DraftStatus: "no_proposal",
		Iterations: 1, TreeDigest: strings.Repeat("d", 64)}
	body, _ := json.Marshal(noProposal)
	if got, err := DecodeInvocationResponse(body, expected); err != nil || got.Status != "no_proposal" || len(got.ProposalData) != 0 {
		t.Fatalf("valid no-proposal response=%+v err=%v", got, err)
	}
}
