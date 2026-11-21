package workspaceruntime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workspacejournal"
)

func TestLocalHTTPProviderLifecycleE2E(t *testing.T) {
	type fakeMicroVM struct {
		mu           sync.Mutex
		state        types.MicrovmState
		request      map[string]any
		runCount     int
		runRequests  int
		failFirstRun bool
		calls        int
	}
	fake := &fakeMicroVM{state: types.MicrovmStatePending, failFirstRun: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.calls++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/2025-09-09/microvms":
			fake.runRequests++
			body, err := io.ReadAll(io.LimitReader(req.Body, 16<<10))
			if err != nil {
				http.Error(w, "read request", http.StatusBadRequest)
				return
			}
			var request map[string]any
			if json.Unmarshal(body, &request) != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if fake.request == nil {
				fake.request = request
				fake.runCount++
			} else if request["clientToken"] != fake.request["clientToken"] || request["imageIdentifier"] != fake.request["imageIdentifier"] {
				http.Error(w, "idempotent retry changed request", http.StatusConflict)
				return
			}
			if fake.failFirstRun {
				fake.failFirstRun = false
				http.Error(w, "simulated ambiguous create response", http.StatusServiceUnavailable)
				return
			}
			fake.state = types.MicrovmStateRunning
			writeProviderJSON(w, map[string]any{"microvmId": "microvm-http-1", "state": "PENDING", "endpoint": "https://microvm.invalid", "imageArn": validSpec().ImageIdentifier, "imageVersion": "3", "executionRoleArn": validSpec().ExecutionRoleARN, "maximumDurationInSeconds": 3600, "startedAt": 1791763200, "egressNetworkConnectors": validSpec().EgressNetworkConnectors, "ingressNetworkConnectors": validSpec().IngressNetworkConnectors})
		case req.Method == http.MethodGet && req.URL.Path == "/2025-09-09/microvms/microvm-http-1":
			writeHTTPMicroVM(w, fake.state)
		case req.Method == http.MethodPost && req.URL.Path == "/2025-09-09/microvms/microvm-http-1/suspend":
			fake.state = types.MicrovmStateSuspended
			writeProviderJSON(w, map[string]any{})
		case req.Method == http.MethodPost && req.URL.Path == "/2025-09-09/microvms/microvm-http-1/resume":
			fake.state = types.MicrovmStateRunning
			writeProviderJSON(w, map[string]any{})
		case req.Method == http.MethodDelete && req.URL.Path == "/2025-09-09/microvms/microvm-http-1":
			fake.state = types.MicrovmStateTerminated
			writeProviderJSON(w, map[string]any{})
		default:
			http.Error(w, "unexpected provider route: "+req.Method+" "+req.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := lambdamicrovms.NewFromConfig(aws.Config{
		Region: "us-east-1", BaseEndpoint: aws.String(server.URL),
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("local-test", "local-test-only", "")),
		HTTPClient:  server.Client(),
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	})
	store := newMemoryStore()
	runtime, err := NewLambdaMicroVM(client, store, resolverFunc(func(context.Context, resourcecontroller.Command) (LaunchSpec, error) { return validSpec(), nil }),
		Config{SupportedRuntimeClasses: map[string]bool{"standard": true}, PollInterval: 10 * time.Millisecond, PollTimeout: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ensure := testCommand(domain.OperationEnsureRunning, 1, "op_http_ensure")
	if _, err := runtime.Execute(ctx, ensure); err == nil {
		t.Fatal("first provider request should model an ambiguous failure")
	}
	// Recreate the adapter as if its process restarted. The resolver changes, but
	// the durable journal must keep the original request and idempotency token.
	changedSpec := validSpec()
	changedSpec.ImageVersion = "99"
	runtime, err = NewLambdaMicroVM(client, store, resolverFunc(func(context.Context, resourcecontroller.Command) (LaunchSpec, error) { return changedSpec, nil }),
		Config{SupportedRuntimeClasses: map[string]bool{"standard": true}, PollInterval: 10 * time.Millisecond, PollTimeout: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if observed, err := runtime.Execute(ctx, ensure); err != nil || observed != domain.ObservedWorkspaceRunning {
		t.Fatalf("ensure after adapter restart = %q, %v", observed, err)
	}
	if observed, err := runtime.Execute(ctx, ensure); err != nil || observed != domain.ObservedWorkspaceRunning {
		t.Fatalf("duplicate ensure via SDK HTTP transport = %q, %v", observed, err)
	}
	if observed, err := runtime.Execute(ctx, testCommand(domain.OperationSuspend, 2, "op_http_suspend")); err != nil || observed != domain.ObservedWorkspaceSuspended {
		t.Fatalf("suspend via SDK HTTP transport = %q, %v", observed, err)
	}
	if observed, err := runtime.Execute(ctx, testCommand(domain.OperationResume, 3, "op_http_resume")); err != nil || observed != domain.ObservedWorkspaceRunning {
		t.Fatalf("resume via SDK HTTP transport = %q, %v", observed, err)
	}
	if observed, err := runtime.Execute(ctx, testCommand(domain.OperationDelete, 4, "op_http_delete")); err != nil || observed != domain.ObservedWorkspaceDeleted {
		t.Fatalf("delete via SDK HTTP transport = %q, %v", observed, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.runCount != 1 || fake.runRequests != 2 || fake.request["imageIdentifier"] != validSpec().ImageIdentifier || fake.request["imageVersion"] != "3" || strings.TrimSpace(fake.request["clientToken"].(string)) == "" {
		t.Fatalf("unexpected fake provider create count/request: allocations=%d requests=%d request=%v", fake.runCount, fake.runRequests, fake.request)
	}
	if fake.state != types.MicrovmStateTerminated {
		t.Fatalf("provider ended in state %q", fake.state)
	}
}

func TestHTTPProviderIdentityMismatchIsNeverAdoptedOrMutated(t *testing.T) {
	var creates, gets, deletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/2025-09-09/microvms":
			creates++
			writeProviderJSON(w, map[string]any{"microvmId": "microvm-owned", "state": "PENDING", "endpoint": "https://microvm.invalid", "imageArn": validSpec().ImageIdentifier, "imageVersion": "3", "executionRoleArn": validSpec().ExecutionRoleARN, "maximumDurationInSeconds": 3600, "startedAt": 1791763200, "egressNetworkConnectors": validSpec().EgressNetworkConnectors, "ingressNetworkConnectors": validSpec().IngressNetworkConnectors})
		case req.Method == http.MethodGet && req.URL.Path == "/2025-09-09/microvms/microvm-owned":
			gets++
			writeHTTPMicroVMWithID(w, "microvm-foreign", types.MicrovmStateRunning)
		case req.Method == http.MethodDelete:
			deletes++
			writeProviderJSON(w, map[string]any{})
		default:
			http.Error(w, "unexpected provider route", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := lambdamicrovms.NewFromConfig(aws.Config{Region: "us-east-1", BaseEndpoint: aws.String(server.URL),
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("local-test", "local-test-only", "")), HTTPClient: server.Client(),
		Retryer: func() aws.Retryer { return aws.NopRetryer{} }})
	runtime, err := NewLambdaMicroVM(client, newMemoryStore(), resolverFunc(func(context.Context, resourcecontroller.Command) (LaunchSpec, error) { return validSpec(), nil }),
		Config{SupportedRuntimeClasses: map[string]bool{"standard": true}, PollInterval: 10 * time.Millisecond, PollTimeout: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Execute(context.Background(), testCommand(domain.OperationEnsureRunning, 1, "op_foreign")); err == nil {
		t.Fatal("adapter accepted a provider response for an unbound MicroVM")
	}
	if creates != 1 || gets != 1 || deletes != 0 {
		t.Fatalf("foreign provider identity was adopted or mutated: creates=%d gets=%d deletes=%d", creates, gets, deletes)
	}
}

func TestHTTPProviderTimeoutKeepsJournalForRestartRetry(t *testing.T) {
	var mu sync.Mutex
	getBlocked := true
	runCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/2025-09-09/microvms":
			mu.Lock()
			runCount++
			mu.Unlock()
			writeProviderJSON(w, map[string]any{"microvmId": "microvm-timeout", "state": "PENDING", "endpoint": "https://microvm.invalid", "imageArn": validSpec().ImageIdentifier, "imageVersion": "3", "executionRoleArn": validSpec().ExecutionRoleARN, "maximumDurationInSeconds": 3600, "startedAt": 1791763200, "egressNetworkConnectors": validSpec().EgressNetworkConnectors, "ingressNetworkConnectors": validSpec().IngressNetworkConnectors})
		case req.Method == http.MethodGet && req.URL.Path == "/2025-09-09/microvms/microvm-timeout":
			mu.Lock()
			blocked := getBlocked
			mu.Unlock()
			if blocked {
				<-req.Context().Done()
				return
			}
			writeHTTPMicroVMWithID(w, "microvm-timeout", types.MicrovmStateRunning)
		default:
			http.Error(w, "unexpected provider route", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := lambdamicrovms.NewFromConfig(aws.Config{Region: "us-east-1", BaseEndpoint: aws.String(server.URL),
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("local-test", "local-test-only", "")), HTTPClient: server.Client(),
		Retryer: func() aws.Retryer { return aws.NopRetryer{} }})
	store := newMemoryStore()
	command := testCommand(domain.OperationEnsureRunning, 1, "op_http_timeout")
	resolver := resolverFunc(func(context.Context, resourcecontroller.Command) (LaunchSpec, error) { return validSpec(), nil })
	newAdapter := func(specResolver resolverFunc, timeout time.Duration) *LambdaMicroVM {
		runtime, err := NewLambdaMicroVM(client, store, specResolver, Config{SupportedRuntimeClasses: map[string]bool{"standard": true}, PollInterval: 10 * time.Millisecond, PollTimeout: timeout}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	if _, err := newAdapter(resolver, 30*time.Millisecond).Execute(context.Background(), command); err == nil {
		t.Fatal("blocked provider Get did not honor the polling deadline")
	}
	pinned, err := store.Get(context.Background(), command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
	if err != nil || pinned.Journal.ProviderID != "microvm-timeout" || pinned.Journal.State != workspacejournal.Provisioning {
		t.Fatalf("provider timeout lost durable allocation identity: %+v, %v", pinned.Journal, err)
	}
	mu.Lock()
	getBlocked = false
	mu.Unlock()
	changed := validSpec()
	changed.ImageVersion = "42"
	if observed, err := newAdapter(func(context.Context, resourcecontroller.Command) (LaunchSpec, error) { return changed, nil }, time.Second).Execute(context.Background(), command); err != nil || observed != domain.ObservedWorkspaceRunning {
		t.Fatalf("retry after provider timeout = %q, %v", observed, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if runCount != 1 {
		t.Fatalf("provider timeout retry allocated more than once: Run calls=%d", runCount)
	}
}

func writeHTTPMicroVM(w http.ResponseWriter, state types.MicrovmState) {
	writeHTTPMicroVMWithID(w, "microvm-http-1", state)
}

func writeHTTPMicroVMWithID(w http.ResponseWriter, id string, state types.MicrovmState) {
	writeProviderJSON(w, map[string]any{"microvmId": id, "state": string(state), "endpoint": "https://microvm.invalid",
		"imageArn": validSpec().ImageIdentifier, "imageVersion": "3", "executionRoleArn": validSpec().ExecutionRoleARN,
		"maximumDurationInSeconds": 3600, "startedAt": 1791763200,
		"egressNetworkConnectors": validSpec().EgressNetworkConnectors, "ingressNetworkConnectors": validSpec().IngressNetworkConnectors})
}

func writeProviderJSON(w http.ResponseWriter, value any) {
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(value)
}
