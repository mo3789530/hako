package workloadcontroller

import (
	"errors"
	"net/http"
	"time"

	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

var ErrLambdaComposition = errors.New("Lambda MicroVM Workload composition is incomplete")

// LambdaWorkloadHandlerConfig contains the trusted dependencies owned by one
// Resource Plane. AWS clients, journal storage, metadata authorization,
// repository checkout, and S3 stores are injected so composition can be tested
// locally without loading real credentials or contacting AWS.
type LambdaWorkloadHandlerConfig struct {
	ResourcePlaneID string
	Journal         workloadjournal.Store
	LambdaAPI       LambdaAPI
	Resolver        Resolver
	SourceArchives  workloadsource.SourceArchiveReader
	AttemptLogs     workloadagent.AttemptLogWriter
	AgentTransport  http.RoundTripper
	Runtime         LambdaConfig
	Now             func() time.Time
}

type LambdaWorkloadRecoveryHandlerConfig struct {
	Workload  LambdaWorkloadHandlerConfig
	Leases    RecoveryLeaseManager
	Publisher ResultPublisher
	Recovery  RecoveryLambdaConfig
}

// NewLambdaWorkloadHandler wires the accepted-command queue path through the
// durable Resource Plane journal, trusted resolver, Lambda MicroVM provider,
// scoped Agent protocol, and partial-batch SQS adapter. It performs no AWS API
// calls during construction. Recovery scheduling is composed separately.
func NewLambdaWorkloadHandler(config LambdaWorkloadHandlerConfig) (*LambdaHandler, error) {
	controller, err := NewLambdaWorkloadController(config)
	if err != nil {
		return nil, err
	}
	handler, err := NewLambdaHandler(controller)
	if err != nil {
		return nil, ErrLambdaComposition
	}
	return handler, nil
}

// NewLambdaWorkloadController builds the shared command/recovery core. It
// makes no AWS calls; concrete AWS clients and authenticated secret material
// are supplied by the deployment-specific binary.
func NewLambdaWorkloadController(config LambdaWorkloadHandlerConfig) (*Controller, error) {
	if config.ResourcePlaneID == "" || config.Journal == nil || config.LambdaAPI == nil || config.Resolver == nil ||
		config.SourceArchives == nil || config.AttemptLogs == nil || config.Runtime.AgentTicketIssuer == nil {
		return nil, ErrLambdaComposition
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	acceptor, err := NewAcceptor(config.Journal, config.ResourcePlaneID, now)
	if err != nil {
		return nil, ErrLambdaComposition
	}
	agent := NewHTTPAgentRunner(workloadagent.NewClient(config.AgentTransport), config.SourceArchives, config.AttemptLogs)
	runtime, err := NewLambdaMicroVM(config.LambdaAPI, config.Journal, config.Resolver, agent, config.Runtime, now)
	if err != nil {
		return nil, ErrLambdaComposition
	}
	controller, err := NewController(acceptor, config.Resolver, runtime)
	if err != nil {
		return nil, ErrLambdaComposition
	}
	return controller, nil
}

// NewLambdaWorkloadRecoveryHandler composes the EventBridge sweep around the
// same journal, resolver, MicroVM, Agent, and result dependencies as the SQS
// command handler. The journal must provide bounded recovery listing; leases
// and publisher remain explicit so a deployment cannot silently skip either.
func NewLambdaWorkloadRecoveryHandler(config LambdaWorkloadRecoveryHandlerConfig) (*RecoveryLambdaHandler, error) {
	if config.Workload.Journal == nil || config.Leases == nil || config.Publisher == nil {
		return nil, ErrLambdaComposition
	}
	lister, ok := config.Workload.Journal.(RecoveryJournalLister)
	if !ok || lister == nil {
		return nil, ErrLambdaComposition
	}
	controller, err := NewLambdaWorkloadController(config.Workload)
	if err != nil {
		return nil, err
	}
	handler, err := NewRecoveryLambdaHandler(controller, lister, config.Leases, config.Publisher, config.Recovery)
	if err != nil {
		return nil, ErrLambdaComposition
	}
	return handler, nil
}
