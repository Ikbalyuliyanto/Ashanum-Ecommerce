package returns

import "testing"

func TestStateMachine(t *testing.T) {
	refundPath := []Status{StatusRequested, StatusApproved, StatusWaitingReturn, StatusItemReceived, StatusRefundPending, StatusRefunded, StatusCompleted}
	exchangePath := []Status{StatusRequested, StatusApproved, StatusWaitingReturn, StatusItemReceived, StatusExchanged, StatusCompleted}
	for typ, path := range map[Type][]Status{TypeRefund: refundPath, TypeExchange: exchangePath} {
		for i := 0; i+1 < len(path); i++ {
			if !allowed(typ, path[i], path[i+1]) {
				t.Errorf("%s: %s → %s must be allowed", typ, path[i], path[i+1])
			}
		}
		// No skipping steps and no going back.
		for i := range path {
			for j := range path {
				if j != i+1 && allowed(typ, path[i], path[j]) {
					t.Errorf("%s: %s → %s must be refused", typ, path[i], path[j])
				}
			}
		}
		if !allowed(typ, StatusRequested, StatusRejected) {
			t.Errorf("%s: REQUESTED → REJECTED must be allowed", typ)
		}
		for _, s := range []Status{StatusRejected, StatusCancelled, StatusCompleted} {
			if len(flow[typ][s]) != 0 {
				t.Errorf("%s: %s must be final", typ, s)
			}
		}
		if allowed(typ, StatusItemReceived, StatusCancelled) {
			t.Errorf("%s: goods already back cannot be cancelled", typ)
		}
	}
	if allowed(TypeExchange, StatusItemReceived, StatusRefundPending) || allowed(TypeRefund, StatusItemReceived, StatusExchanged) {
		t.Error("refund and exchange paths must not mix")
	}
}
