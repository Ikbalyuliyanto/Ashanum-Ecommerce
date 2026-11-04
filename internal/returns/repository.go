package returns

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
	"ecommerce/internal/media"
)

type Repository struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
	tx   bool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, q: dbgen.New(pool)}
}

// InTx runs fn in one transaction (commit on nil, rollback otherwise).
func (r *Repository) InTx(ctx context.Context, fn func(tx *Repository) error) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return fn(&Repository{q: r.q.WithTx(tx), tx: true})
	})
}

type orderRow struct {
	ID          int64
	Number      string
	Status      string
	CompletedAt *time.Time
	Paid        bool // has a PAID payment
}

// Order loads the user's order; inside a transaction the row is locked.
func (r *Repository) Order(ctx context.Context, userID int64, number string) (orderRow, error) {
	var row dbgen.ReturnOrderRow
	var err error
	if r.tx {
		var locked dbgen.LockReturnOrderRow
		locked, err = r.q.LockReturnOrder(ctx, dbgen.LockReturnOrderParams{OrderNumber: number, UserID: userID})
		row = dbgen.ReturnOrderRow(locked)
	} else {
		row, err = r.q.ReturnOrder(ctx, dbgen.ReturnOrderParams{OrderNumber: number, UserID: userID})
	}
	if database.IsNoRows(err) {
		return orderRow{}, ErrNotFound
	}
	if err != nil {
		return orderRow{}, err
	}
	return orderRow{ID: row.ID, Number: row.OrderNumber, Status: row.Status, CompletedAt: row.CompletedAt, Paid: row.Paid}, nil
}

// Items lists the order's items with the quantity already held by returns.
func (r *Repository) Items(ctx context.Context, orderID int64) ([]Item, error) {
	rows, err := r.q.ReturnableItems(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]Item, len(rows))
	for i, row := range rows {
		out[i] = Item{OrderItemID: row.ID, ProductName: row.ProductName, VariantLabel: row.VariantLabel,
			ImageURL: row.ImageUrl, UnitPrice: row.UnitPrice, Purchased: int(row.Quantity), Returned: int(row.Returned)}
	}
	return out, nil
}

// LockItems row-locks the chosen order items in id order.
func (r *Repository) LockItems(ctx context.Context, orderID int64, ids []int64) error {
	_, err := r.q.LockReturnItems(ctx, dbgen.LockReturnItemsParams{OrderID: orderID, Ids: ids})
	return err
}

type newReturn struct {
	OrderID, UserID int64
	Type            Type
	Reason          Reason
	Description     string
	Amount          int64
	Key             string
	Items           []ReturnItem
	Evidence        []media.Attachment
}

// Create writes the return, its items and evidence.
func (r *Repository) Create(ctx context.Context, n newReturn) (int64, string, error) {
	p := dbgen.CreateReturnParams{OrderID: n.OrderID, UserID: n.UserID, Type: string(n.Type), Reason: string(n.Reason),
		Description: n.Description, RefundAmount: n.Amount}
	if n.Key != "" {
		p.IdempotencyKey = &n.Key
	}
	row, err := r.q.CreateReturn(ctx, p)
	if database.IsUniqueViolation(err, "returns_idempotency_key") {
		return 0, "", errDuplicateKey
	}
	if err != nil {
		return 0, "", err
	}
	items := dbgen.AddReturnItemsParams{ReturnID: row.ID}
	for _, it := range n.Items {
		items.OrderItemIds = append(items.OrderItemIds, it.OrderItemID)
		items.Quantities = append(items.Quantities, int32(it.Quantity))
		items.UnitPrices = append(items.UnitPrices, it.UnitPrice)
	}
	if err := r.q.AddReturnItems(ctx, items); err != nil {
		return 0, "", err
	}
	if len(n.Evidence) > 0 {
		ev := dbgen.AddReturnEvidenceParams{ReturnID: row.ID}
		for _, a := range n.Evidence {
			ev.Urls = append(ev.Urls, a.URL)
			ev.MediaTypes = append(ev.MediaTypes, a.Kind)
		}
		if err := r.q.AddReturnEvidence(ctx, ev); err != nil {
			return 0, "", err
		}
	}
	return row.ID, row.ReturnNumber, nil
}

func (r *Repository) NumberByKey(ctx context.Context, userID int64, key string) (string, error) {
	n, err := r.q.ReturnNumberByKey(ctx, dbgen.ReturnNumberByKeyParams{UserID: userID, IdempotencyKey: &key})
	if database.IsNoRows(err) {
		return "", ErrNotFound
	}
	return n, err
}

func (r *Repository) Event(ctx context.Context, returnID int64, from, to Status, actor Actor, note string) error {
	p := dbgen.AddReturnEventParams{ReturnID: returnID, ToStatus: string(to), Actor: string(actor), Note: note}
	if from != "" {
		f := string(from)
		p.FromStatus = &f
	}
	return r.q.AddReturnEvent(ctx, p)
}

func (r *Repository) List(ctx context.Context, userID int64, limit, offset int) ([]Summary, error) {
	rows, err := r.q.ListReturns(ctx, dbgen.ListReturnsParams{UserID: userID, RowLimit: int32(limit), RowOffset: int32(offset)})
	if err != nil {
		return nil, err
	}
	out := make([]Summary, len(rows))
	for i, row := range rows {
		out[i] = Summary{Number: row.ReturnNumber, OrderNumber: row.OrderNumber, Type: Type(row.Type), Status: Status(row.Status),
			RefundAmount: row.RefundAmount, CreatedAt: row.CreatedAt, ItemCount: int(row.ItemCount),
			ProductName: row.ProductName, ImageURL: row.ImageUrl}
	}
	return out, nil
}

func (r *Repository) OrderReturns(ctx context.Context, userID, orderID int64) ([]OrderReturn, error) {
	rows, err := r.q.OrderReturns(ctx, dbgen.OrderReturnsParams{OrderID: orderID, UserID: userID})
	if err != nil {
		return nil, err
	}
	out := make([]OrderReturn, len(rows))
	for i, row := range rows {
		out[i] = OrderReturn{Number: row.ReturnNumber, Type: Type(row.Type), Status: Status(row.Status), CreatedAt: row.CreatedAt}
	}
	return out, nil
}

// Get loads one of the user's returns with items, evidence, timeline and
// refunds.
func (r *Repository) Get(ctx context.Context, userID int64, number string) (Return, error) {
	row, err := r.q.GetReturn(ctx, dbgen.GetReturnParams{ReturnNumber: number, UserID: userID})
	if database.IsNoRows(err) {
		return Return{}, ErrNotFound
	}
	if err != nil {
		return Return{}, err
	}
	ret := Return{
		ID: row.ID, Number: row.ReturnNumber, OrderNumber: row.OrderNumber, Type: Type(row.Type), Status: Status(row.Status),
		Reason: Reason(row.Reason), Description: row.Description, RefundAmount: row.RefundAmount,
		ResolutionNote: row.ResolutionNote, RequestedAt: row.RequestedAt, ApprovedAt: row.ApprovedAt,
		RejectedAt: row.RejectedAt, CompletedAt: row.CompletedAt,
	}
	return ret, r.details(ctx, &ret)
}

// AdminGet loads any return by number (admin), with the buyer's contact.
func (r *Repository) AdminGet(ctx context.Context, number string) (Return, error) {
	row, err := r.q.AdminReturn(ctx, number)
	if database.IsNoRows(err) {
		return Return{}, ErrNotFound
	}
	if err != nil {
		return Return{}, err
	}
	ret := Return{
		ID: row.ID, Number: row.ReturnNumber, OrderNumber: row.OrderNumber, OrderTotal: row.OrderTotal,
		BuyerEmail: row.Email, BuyerName: joinName(row.FirstName, row.LastName),
		Type: Type(row.Type), Status: Status(row.Status), Reason: Reason(row.Reason), Description: row.Description,
		RefundAmount: row.RefundAmount, ResolutionNote: row.ResolutionNote, RequestedAt: row.RequestedAt,
		ApprovedAt: row.ApprovedAt, RejectedAt: row.RejectedAt, CompletedAt: row.CompletedAt,
	}
	return ret, r.details(ctx, &ret)
}

func joinName(first, last string) string {
	switch {
	case first == "":
		return last
	case last == "":
		return first
	}
	return first + " " + last
}

// details adds items, evidence, timeline and refunds (four small queries).
func (r *Repository) details(ctx context.Context, ret *Return) error {
	items, err := r.q.ReturnItemsOf(ctx, ret.ID)
	if err != nil {
		return err
	}
	for _, it := range items {
		ri := ReturnItem{ID: it.ID, OrderItemID: it.OrderItemID, ProductID: deref(it.ProductID), VariantID: deref(it.VariantID),
			ProductName: it.ProductName, SKU: it.Sku, VariantLabel: it.VariantLabel, ImageURL: it.ImageUrl,
			Quantity: int(it.Quantity), UnitPrice: it.UnitPrice, InspectedAt: it.InspectedAt, RestockedAt: it.RestockedAt,
			Condition: Condition(deref(it.Condition)), Restocked: int(deref(it.RestockedQuantity)),
			ReplacementSKU: deref(it.ReplacementSku), ReplacementLabel: deref(it.ReplacementLabel),
			ReplacementQty: int(deref(it.ReplacementQuantity))}
		if it.ApprovedQuantity != nil {
			n := int(*it.ApprovedQuantity)
			ri.Approved = &n
		}
		ret.Items = append(ret.Items, ri)
	}
	evidence, err := r.q.ReturnEvidenceOf(ctx, ret.ID)
	if err != nil {
		return err
	}
	for _, e := range evidence {
		ret.Evidence = append(ret.Evidence, media.Attachment{URL: e.Url, Kind: e.MediaType})
	}
	events, err := r.q.ReturnEventsOf(ctx, ret.ID)
	if err != nil {
		return err
	}
	for _, e := range events {
		ev := Event{To: Status(e.ToStatus), Actor: Actor(e.Actor), Note: e.Note, At: e.CreatedAt}
		if e.FromStatus != nil {
			ev.From = Status(*e.FromStatus)
		}
		ret.Events = append(ret.Events, ev)
	}
	refunds, err := r.q.ReturnRefunds(ctx, &ret.ID)
	if err != nil {
		return err
	}
	for _, f := range refunds {
		ret.Refunds = append(ret.Refunds, refundFrom(f))
	}
	if n := len(ret.Refunds); n > 0 {
		ret.Refund = &ret.Refunds[n-1]
	}
	return nil
}

func refundFrom(f dbgen.Refund) Refund {
	return Refund{ID: f.ID, Source: RefundSource(f.Source), Amount: f.Amount, Status: RefundStatus(f.Status),
		Provider: deref(f.Provider), Reference: deref(f.ProviderRefundID), Note: f.Note,
		FailureReason: deref(f.FailureReason), RequestedAt: f.RequestedAt, ProcessedAt: f.ProcessedAt}
}

// locked is a return row held for a status change.
type locked struct {
	ID         int64
	Number     string
	OrderID    int64
	Type       Type
	Status     Status
	Amount     int64
	PaymentID  *int64
	PaidAmount int64
}

// Lock row-locks a return by id (admin side).
func (r *Repository) Lock(ctx context.Context, id int64) (locked, error) {
	row, err := r.q.LockReturn(ctx, id)
	if database.IsNoRows(err) {
		return locked{}, ErrNotFound
	}
	if err != nil {
		return locked{}, err
	}
	return lockedFrom(dbgen.LockReturnByNumberRow(row)), nil
}

// LockOwn row-locks one of the user's returns by number.
func (r *Repository) LockOwn(ctx context.Context, userID int64, number string) (locked, error) {
	row, err := r.q.LockReturnByNumber(ctx, dbgen.LockReturnByNumberParams{ReturnNumber: number, UserID: userID})
	if database.IsNoRows(err) {
		return locked{}, ErrNotFound
	}
	if err != nil {
		return locked{}, err
	}
	return lockedFrom(row), nil
}

func lockedFrom(row dbgen.LockReturnByNumberRow) locked {
	l := locked{ID: row.ID, Number: row.ReturnNumber, OrderID: row.OrderID, Type: Type(row.Type),
		Status: Status(row.Status), Amount: row.RefundAmount, PaymentID: row.PaymentID}
	if row.PaidAmount != nil {
		l.PaidAmount = *row.PaidAmount
	}
	return l
}

// SetStatus moves a locked return from → to; false if it was not in from.
func (r *Repository) SetStatus(ctx context.Context, id int64, from, to Status, note string) (bool, error) {
	n, err := r.q.SetReturnStatus(ctx, dbgen.SetReturnStatusParams{ID: id, FromStatus: string(from), ToStatus: string(to), Note: note})
	return n == 1, err
}

// line is a locked return line with its order item.
type line struct {
	ID          int64
	OrderItemID int64
	Quantity    int
	UnitPrice   int64
	Approved    *int
	Condition   Condition
	Restocked   bool
	Replaced    bool
	ProductID   int64
	VariantID   int64
	ProductName string
	SKU         string
	Label       string
}

func (l line) approved() int {
	if l.Approved != nil {
		return *l.Approved
	}
	return l.Quantity
}

// Lines row-locks the return's lines (id order).
func (r *Repository) Lines(ctx context.Context, returnID int64) ([]line, error) {
	rows, err := r.q.LockReturnLines(ctx, returnID)
	if err != nil {
		return nil, err
	}
	out := make([]line, len(rows))
	for i, row := range rows {
		out[i] = line{ID: row.ID, OrderItemID: row.OrderItemID, Quantity: int(row.Quantity), UnitPrice: row.UnitPrice,
			Condition: Condition(deref(row.Condition)), Restocked: row.RestockedAt != nil, Replaced: row.ReplacementQuantity != nil,
			ProductID: deref(row.ProductID), VariantID: deref(row.VariantID), ProductName: row.ProductName,
			SKU: row.Sku, Label: row.VariantLabel}
		if row.ApprovedQuantity != nil {
			n := int(*row.ApprovedQuantity)
			out[i].Approved = &n
		}
	}
	return out, nil
}

func (r *Repository) SetApproved(ctx context.Context, returnID int64, ids []int64, quantities []int32) error {
	return r.q.SetApprovedQuantities(ctx, dbgen.SetApprovedQuantitiesParams{ReturnID: returnID, Ids: ids, Quantities: quantities})
}

func (r *Repository) SetAmount(ctx context.Context, returnID, amount int64) error {
	return r.q.SetReturnAmount(ctx, dbgen.SetReturnAmountParams{ID: returnID, RefundAmount: amount})
}

func (r *Repository) Inspect(ctx context.Context, returnID int64, ids []int64, conditions []string) error {
	return r.q.InspectReturnItems(ctx, dbgen.InspectReturnItemsParams{ReturnID: returnID, Ids: ids, Conditions: conditions})
}

// MarkRestocked records a line's restock; false when it was already done.
func (r *Repository) MarkRestocked(ctx context.Context, lineID int64, quantity int) (bool, error) {
	n, err := r.q.MarkRestocked(ctx, dbgen.MarkRestockedParams{ID: lineID, Quantity: ptr(int32(quantity))})
	return n == 1, err
}

// SetReplacement records a line's replacement; false when already set.
func (r *Repository) SetReplacement(ctx context.Context, lineID int64, v Replacement, quantity int) (bool, error) {
	n, err := r.q.SetReplacement(ctx, dbgen.SetReplacementParams{ID: lineID, VariantID: &v.VariantID, Sku: &v.SKU,
		Label: &v.VariantLabel, Quantity: ptr(int32(quantity))})
	return n == 1, err
}

// Replacements lists the active variants of the given products.
func (r *Repository) Replacements(ctx context.Context, productIDs []int64) ([]Replacement, error) {
	rows, err := r.q.ReplacementVariants(ctx, productIDs)
	if err != nil {
		return nil, err
	}
	out := make([]Replacement, len(rows))
	for i, row := range rows {
		out[i] = Replacement{VariantID: row.ID, ProductID: row.ProductID, SKU: row.Sku, VariantLabel: row.VariantLabel, Stock: int(row.Stock)}
	}
	return out, nil
}

func (r *Repository) IDByNumber(ctx context.Context, number string) (int64, error) {
	id, err := r.q.ReturnIDByNumber(ctx, number)
	if database.IsNoRows(err) {
		return 0, ErrNotFound
	}
	return id, err
}

func (r *Repository) AdminList(ctx context.Context, statuses []string, q string, limit, offset int) ([]Summary, error) {
	rows, err := r.q.AdminReturns(ctx, dbgen.AdminReturnsParams{Statuses: statuses, Q: q, RowLimit: int32(limit), RowOffset: int32(offset)})
	if err != nil {
		return nil, err
	}
	out := make([]Summary, len(rows))
	for i, row := range rows {
		out[i] = Summary{Number: row.ReturnNumber, OrderNumber: row.OrderNumber, Type: Type(row.Type), Status: Status(row.Status),
			RefundAmount: row.RefundAmount, CreatedAt: row.CreatedAt, ItemCount: int(row.ItemCount),
			ProductName: row.ProductName, ImageURL: row.ImageUrl}
	}
	return out, nil
}

// --- refunds

// LockPayment row-locks a payment; refunds of one payment are serialized
// on it, so their total cannot pass the amount paid.
func (r *Repository) LockPayment(ctx context.Context, paymentID int64) (int64, error) {
	p, err := r.q.LockRefundPayment(ctx, paymentID)
	if database.IsNoRows(err) {
		return 0, ErrNoPayment
	}
	return p.Amount, err
}

func (r *Repository) RefundedForPayment(ctx context.Context, paymentID int64) (int64, error) {
	return r.q.RefundedForPayment(ctx, &paymentID)
}

func (r *Repository) HasActiveRefund(ctx context.Context, returnID int64) (bool, error) {
	return r.q.HasActiveRefund(ctx, &returnID)
}

func (r *Repository) CreateRefund(ctx context.Context, returnID, orderID int64, paymentID *int64, amount int64) (Refund, error) {
	f, err := r.q.CreateRefund(ctx, dbgen.CreateRefundParams{ReturnID: &returnID, OrderID: orderID, PaymentID: paymentID, Amount: amount})
	if err != nil {
		return Refund{}, err
	}
	return refundFrom(f), nil
}

// OpenRefund row-locks the return's refund that is not finished yet.
func (r *Repository) OpenRefund(ctx context.Context, returnID int64) (int64, error) {
	f, err := r.q.OpenRefund(ctx, &returnID)
	if database.IsNoRows(err) {
		return 0, ErrInvalidTransition
	}
	return f.ID, err
}

func (r *Repository) RecordRefund(ctx context.Context, refundID int64, res RefundResult) (bool, error) {
	p := dbgen.RecordRefundResultParams{ID: refundID, Provider: &res.Provider, Note: res.Note}
	if res.Reference != "" {
		p.ProviderRefundID = &res.Reference
	}
	n, err := r.q.RecordRefundResult(ctx, p)
	return n == 1, err
}

type lockedRefund struct {
	ID           int64
	ReturnID     int64 // 0 for an order cancellation refund
	OrderID      int64
	Source       RefundSource
	Status       RefundStatus
	Amount       int64
	ReturnStatus Status
}

func (r *Repository) LockRefund(ctx context.Context, id int64) (lockedRefund, error) {
	row, err := r.q.LockRefund(ctx, id)
	if database.IsNoRows(err) {
		return lockedRefund{}, ErrRefundNotFound
	}
	if err != nil {
		return lockedRefund{}, err
	}
	return lockedRefund{ID: row.ID, ReturnID: deref(row.ReturnID), OrderID: row.OrderID, Source: RefundSource(row.Source),
		Status: RefundStatus(row.Status), Amount: row.Amount, ReturnStatus: Status(deref(row.ReturnStatus))}, nil
}

// RefundReturnID is the return a refund belongs to (0 for a cancellation).
func (r *Repository) RefundReturnID(ctx context.Context, id int64) (int64, error) {
	rid, err := r.q.RefundReturnID(ctx, id)
	if database.IsNoRows(err) {
		return 0, ErrRefundNotFound
	}
	return deref(rid), err
}

func (r *Repository) MarkRefundProcessing(ctx context.Context, id int64, note string) (bool, error) {
	n, err := r.q.MarkRefundProcessing(ctx, dbgen.MarkRefundProcessingParams{ID: id, Note: note})
	return n == 1, err
}

func (r *Repository) FailRefund(ctx context.Context, id int64, reason string) (bool, error) {
	n, err := r.q.FailRefund(ctx, dbgen.FailRefundParams{ID: id, Reason: &reason})
	return n == 1, err
}

func (r *Repository) AdminRefunds(ctx context.Context, statuses []string, q string, limit, offset int) ([]Refund, error) {
	rows, err := r.q.AdminRefunds(ctx, dbgen.AdminRefundsParams{Statuses: statuses, Q: q, RowLimit: int32(limit), RowOffset: int32(offset)})
	if err != nil {
		return nil, err
	}
	out := make([]Refund, len(rows))
	for i, row := range rows {
		out[i] = Refund{ID: row.ID, Source: RefundSource(row.Source), Status: RefundStatus(row.Status), Amount: row.Amount,
			RequestedAt: row.RequestedAt, ProcessedAt: row.ProcessedAt, Provider: deref(row.Provider),
			Reference: deref(row.ProviderRefundID), OrderNumber: row.OrderNumber, ReturnNumber: deref(row.ReturnNumber),
			PaymentOrderID: deref(row.ProviderOrderID)}
	}
	return out, nil
}

func (r *Repository) AdminRefund(ctx context.Context, id int64) (Refund, error) {
	row, err := r.q.AdminRefund(ctx, id)
	if database.IsNoRows(err) {
		return Refund{}, ErrRefundNotFound
	}
	if err != nil {
		return Refund{}, err
	}
	f := refundFrom(dbgen.Refund{ID: row.ID, ReturnID: row.ReturnID, PaymentID: row.PaymentID, Amount: row.Amount,
		Status: row.Status, Provider: row.Provider, ProviderRefundID: row.ProviderRefundID, RequestedAt: row.RequestedAt,
		ProcessedAt: row.ProcessedAt, FailureReason: row.FailureReason, Source: row.Source, Note: row.Note})
	f.OrderNumber, f.ReturnNumber, f.ReturnStatus = row.OrderNumber, deref(row.ReturnNumber), Status(deref(row.ReturnStatus))
	f.PaymentProvider, f.PaymentOrderID = deref(row.PaymentProvider), deref(row.ProviderOrderID)
	f.PaymentTxID, f.PaymentType, f.PaidAmount = deref(row.ProviderTransactionID), deref(row.PaymentType), deref(row.PaidAmount)
	return f, nil
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func ptr[T any](v T) *T { return &v }
