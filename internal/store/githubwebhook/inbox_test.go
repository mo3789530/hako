package githubwebhook

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactedStoredPayloadRemovesIssueCommentBody(t *testing.T) {
	payload := []byte(`{"action":"created","repository":{"id":1},"comment":{"id":2,"body":"private issue text","author_association":"MEMBER"}}`)
	got, err := redactedStoredPayload("issue_comment", payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "private issue text") {
		t.Fatalf("stored payload contains free-form comment body: %s", got)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("redacted payload is invalid JSON: %v", err)
	}
	var comment map[string]json.RawMessage
	if err := json.Unmarshal(decoded["comment"], &comment); err != nil {
		t.Fatal(err)
	}
	if _, ok := comment["body"]; ok || string(comment["id"]) != "2" || string(comment["author_association"]) != `"MEMBER"` {
		t.Fatalf("stored comment metadata = %s", decoded["comment"])
	}
}

func TestRedactedStoredPayloadLeavesOtherEventsByteIdentical(t *testing.T) {
	payload := []byte(` { "ref" : "refs/heads/main" } `)
	got, err := redactedStoredPayload("push", payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("non-comment payload changed: got %q want %q", got, payload)
	}
}

func TestRedactedStoredPayloadRejectsMalformedIssueComment(t *testing.T) {
	if _, err := redactedStoredPayload("issue_comment", []byte(`{"comment":null}`)); err == nil {
		t.Fatalf("malformed issue comment payload error = %v", err)
	}
}
