package returns

// flow is the return state machine. A status change not listed here is
// refused (ErrInvalidTransition):
//
//	REFUND:   REQUESTED → APPROVED → WAITING_RETURN → ITEM_RECEIVED
//	          → REFUND_PENDING → REFUNDED → COMPLETED
//	EXCHANGE: REQUESTED → APPROVED → WAITING_RETURN → ITEM_RECEIVED
//	          → EXCHANGED → COMPLETED
//	both:     REQUESTED → REJECTED; REQUESTED | APPROVED | WAITING_RETURN
//	          → CANCELLED (before the goods are back with the shop)
var flow = map[Type]map[Status][]Status{
	TypeRefund: {
		StatusRequested:     {StatusApproved, StatusRejected, StatusCancelled},
		StatusApproved:      {StatusWaitingReturn, StatusCancelled},
		StatusWaitingReturn: {StatusItemReceived, StatusCancelled},
		StatusItemReceived:  {StatusRefundPending},
		StatusRefundPending: {StatusRefunded},
		StatusRefunded:      {StatusCompleted},
	},
	TypeExchange: {
		StatusRequested:     {StatusApproved, StatusRejected, StatusCancelled},
		StatusApproved:      {StatusWaitingReturn, StatusCancelled},
		StatusWaitingReturn: {StatusItemReceived, StatusCancelled},
		StatusItemReceived:  {StatusExchanged},
		StatusExchanged:     {StatusCompleted},
	},
}

// allowed reports whether a return of type t may move from → to.
func allowed(t Type, from, to Status) bool {
	for _, s := range flow[t][from] {
		if s == to {
			return true
		}
	}
	return false
}
