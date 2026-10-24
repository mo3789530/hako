package previewcleanup

import (
	"errors"
	"testing"
)

func TestClassifyUsesOnlyBoundedCleanupCategories(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{ErrBindingMissing, "preview_binding_missing"},
		{ErrGatewayRouteRemoval, "gateway_route_remove_failed"},
		{ErrMicroVMTermination, "microvm_terminate_failed"},
		{errors.New("provider error contains a secret"), "provider_unavailable"},
	} {
		if got := classify(test.err); got != test.want {
			t.Errorf("classify(%v) = %q, want %q", test.err, got, test.want)
		}
	}
}
