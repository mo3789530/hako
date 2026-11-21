package workloadcontroller

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

type RecoveryLambdaConfig struct {
	PageSize int32
	MaxPages int
	LeaseTTL time.Duration
}

// RecoveryLambdaHandler adapts an EventBridge schedule to the bounded,
// distributed-lease recovery sweep. Provider/store composition remains in the
// Resource Plane binary so this adapter is easy to exercise without AWS.
type RecoveryLambdaHandler struct {
	controller *Controller
	lister     RecoveryJournalLister
	leases     RecoveryLeaseManager
	publisher  ResultPublisher
	config     RecoveryLambdaConfig
}

func NewRecoveryLambdaHandler(controller *Controller, lister RecoveryJournalLister, leases RecoveryLeaseManager,
	publisher ResultPublisher, config RecoveryLambdaConfig) (*RecoveryLambdaHandler, error) {
	if controller == nil || lister == nil || leases == nil || publisher == nil || config.PageSize < 1 || config.PageSize > 1000 ||
		config.MaxPages < 1 || config.MaxPages > 1000 || config.LeaseTTL < time.Second || config.LeaseTTL > 30*time.Minute {
		return nil, errors.New("bounded Workload recovery Lambda dependencies and limits are required")
	}
	return &RecoveryLambdaHandler{controller: controller, lister: lister, leases: leases, publisher: publisher, config: config}, nil
}

func (h *RecoveryLambdaHandler) Handle(ctx context.Context, _ events.CloudWatchEvent) (RecoverySweepReport, error) {
	if h == nil || h.controller == nil || ctx == nil {
		return RecoverySweepReport{}, errors.New("context and Workload recovery Lambda are required")
	}
	return h.controller.RecoverSweepWithLease(ctx, h.lister, h.leases, h.config.PageSize, h.config.MaxPages, h.config.LeaseTTL, h.publisher)
}

var _ interface {
	Handle(context.Context, events.CloudWatchEvent) (RecoverySweepReport, error)
} = (*RecoveryLambdaHandler)(nil)
