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

// Items lists the order's items with the quantity already returned.
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
// latest refund.
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
	items, err := r.q.ReturnItemsOf(ctx, row.ID)
	if err != nil {
		return Return{}, err
	}
	for _, it := range items {
		ret.Items = append(ret.Items, ReturnItem{OrderItemID: it.OrderItemID, ProductName: it.ProductName,
			VariantLabel: it.VariantLabel, ImageURL: it.ImageUrl, Quantity: int(it.Quantity), UnitPrice: it.UnitPrice})
	}
	evidence, err := r.q.ReturnEvidenceOf(ctx, row.ID)
	if err != nil {
		return Return{}, err
	}
	for _, e := range evidence {
		ret.Evidence = append(ret.Evidence, media.Attachment{URL: e.Url, Kind: e.MediaType})
	}
	events, err := r.q.ReturnEventsOf(ctx, row.ID)
	if err != nil {
		return Return{}, err
	}
	for _, e := range events {
		ev := Event{To: Status(e.ToStatus), Actor: Actor(e.Actor), Note: e.Note, At: e.CreatedAt}
		if e.FromStatus != nil {
			ev.From = Status(*e.FromStatus)
		}
		ret.Events = append(ret.Events, ev)
	}
	refunds, err := r.q.ReturnRefunds(ctx, row.ID)
	if err != nil {
		return Return{}, err
	}
	if n := len(refunds); n > 0 {
		f := refunds[n-1]
		ret.Refund = &Refund{ID: f.ID, Amount: f.Amount, Status: RefundStatus(f.Status), RequestedAt: f.RequestedAt, ProcessedAt: f.ProcessedAt}
		if f.Provider != nil {
			ret.Refund.Provider = *f.Provider
		}
	}
	return ret, nil
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

func (r *Repository) RefundedForPayment(ctx context.Context, paymentID int64) (int64, error) {
	return r.q.RefundedForPayment(ctx, &paymentID)
}

func (r *Repository) CreateRefund(ctx context.Context, returnID int64, paymentID *int64, amount int64) (Refund, error) {
	f, err := r.q.CreateRefund(ctx, dbgen.CreateRefundParams{ReturnID: returnID, PaymentID: paymentID, Amount: amount})
	if err != nil {
		return Refund{}, err
	}
	return Refund{ID: f.ID, Amount: f.Amount, Status: RefundStatus(f.Status), RequestedAt: f.RequestedAt}, nil
}

// OpenRefund row-locks the return's refund that is not finished yet.
func (r *Repository) OpenRefund(ctx context.Context, returnID int64) (int64, error) {
	f, err := r.q.OpenRefund(ctx, returnID)
	if database.IsNoRows(err) {
		return 0, ErrInvalidTransition
	}
	return f.ID, err
}

func (r *Repository) RecordRefund(ctx context.Context, refundID int64, res RefundResult) (bool, error) {
	p := dbgen.RecordRefundResultParams{ID: refundID, Provider: &res.Provider}
	if res.Reference != "" {
		p.ProviderRefundID = &res.Reference
	}
	n, err := r.q.RecordRefundResult(ctx, p)
	return n == 1, err
}
