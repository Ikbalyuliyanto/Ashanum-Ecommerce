package order

import (
	"context"

	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
)

type Repository struct {
	q *dbgen.Queries
}

func NewRepository(db dbgen.DBTX) *Repository { return &Repository{q: dbgen.New(db)} }

// List returns up to limit order summaries for the user, one query.
func (r *Repository) List(ctx context.Context, userID int64, statuses []Status, limit, offset int) ([]Summary, error) {
	st := make([]string, len(statuses))
	for i, s := range statuses {
		st[i] = string(s)
	}
	rows, err := r.q.ListOrders(ctx, dbgen.ListOrdersParams{
		UserID:    userID,
		Statuses:  st,
		RowLimit:  int32(limit),
		RowOffset: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Summary, len(rows))
	for i, row := range rows {
		out[i] = Summary{
			ID:        row.ID,
			Number:    row.OrderNumber,
			Status:    Status(row.Status),
			Total:     row.Total,
			CreatedAt: row.CreatedAt,
			DueAt:     row.PaymentDueAt,
			ItemCount: int(row.ItemCount),
			First: Item{
				ProductName:  row.ProductName,
				VariantLabel: row.VariantLabel,
				ImageURL:     row.ImageUrl,
				Quantity:     int(row.Quantity),
				UnitPrice:    row.UnitPrice,
			},
		}
	}
	return out, nil
}

// Get returns one order of the user with its items, their reviews and
// returned quantities, and the shipment (two queries).
func (r *Repository) Get(ctx context.Context, userID int64, number string) (Order, error) {
	row, err := r.q.GetOrder(ctx, dbgen.GetOrderParams{OrderNumber: number, UserID: userID})
	if database.IsNoRows(err) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, err
	}
	o := Order{
		ID:           row.ID,
		Number:       row.OrderNumber,
		Status:       Status(row.Status),
		Subtotal:     row.Subtotal,
		ShippingCost: row.ShippingCost,
		Discount:     row.Discount,
		Total:        row.Total,
		CreatedAt:    row.CreatedAt,
		PaymentDueAt: row.PaymentDueAt,
		PaidAt:       row.PaidAt,
		Paid:         row.Paid,
		ConfirmedAt:  row.ConfirmedAt,
		CancelledAt:  row.CancelledAt,
		CompletedAt:  row.CompletedAt,
		Address: Address{
			Label:         row.AddressLabel,
			RecipientName: row.RecipientName,
			Phone:         row.RecipientPhone,
			Province:      row.Province,
			City:          row.City,
			District:      row.District,
			Village:       row.Village,
			PostalCode:    row.PostalCode,
			Street:        row.Address,
			Latitude:      row.Latitude,
			Longitude:     row.Longitude,
		},
	}
	if row.CancelReason != nil {
		o.CancelReason = *row.CancelReason
	}
	if row.CourierName != nil && row.ShippedAt != nil {
		o.Shipment = &Shipment{Courier: *row.CourierName, Service: deref(row.ServiceName), Tracking: deref(row.TrackingNumber),
			ShippedAt: *row.ShippedAt}
	}
	var err2 error
	if o.Items, err2 = r.Items(ctx, row.ID); err2 != nil {
		return Order{}, err2
	}
	return o, nil
}

// Items lists an order's lines with the buyer's review and the quantity
// held by returns (one query). The caller must already know the order is
// one the reader may see.
func (r *Repository) Items(ctx context.Context, orderID int64) ([]Item, error) {
	rows, err := r.q.ListOrderItems(ctx, orderID)
	if err != nil {
		return nil, err
	}
	items := make([]Item, len(rows))
	for i, it := range rows {
		items[i] = Item{
			ID:           it.ID,
			ProductID:    deref(it.ProductID),
			VariantID:    deref(it.VariantID),
			ProductName:  it.ProductName,
			ProductSlug:  it.ProductSlug,
			SKU:          it.Sku,
			VariantLabel: it.VariantLabel,
			ImageURL:     it.ImageUrl,
			UnitPrice:    it.UnitPrice,
			Quantity:     int(it.Quantity),
			Subtotal:     it.Subtotal,
			ReviewID:     deref(it.ReviewID),
			Returned:     int(it.Returned),
		}
	}
	return items, nil
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
