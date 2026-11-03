package address

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
)

type Repository struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, q: dbgen.New(pool)}
}

// InTx runs fn with a repository bound to one transaction; it commits when
// fn returns nil and rolls back otherwise.
func (r *Repository) InTx(ctx context.Context, fn func(tx *Repository) error) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return fn(&Repository{q: r.q.WithTx(tx)})
	})
}

func (r *Repository) List(ctx context.Context, userID int64) ([]Address, error) {
	rows, err := r.q.ListAddresses(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Address, len(rows))
	for i, row := range rows {
		out[i] = fromRow(row)
	}
	return out, nil
}

func (r *Repository) Get(ctx context.Context, userID, id int64) (Address, error) {
	row, err := r.q.GetAddress(ctx, dbgen.GetAddressParams{ID: id, UserID: userID})
	if database.IsNoRows(err) {
		return Address{}, ErrNotFound
	}
	if err != nil {
		return Address{}, err
	}
	return fromRow(row), nil
}

func (r *Repository) Count(ctx context.Context, userID int64) (int64, error) {
	return r.q.CountAddresses(ctx, userID)
}

// LockUser serializes address writes of one user within a transaction.
func (r *Repository) LockUser(ctx context.Context, userID int64) error {
	_, err := r.q.LockUserAddresses(ctx, userID)
	if database.IsNoRows(err) {
		return ErrNotFound
	}
	return err
}

func (r *Repository) Create(ctx context.Context, userID int64, f fields, primary bool) (Address, error) {
	row, err := r.q.CreateAddress(ctx, dbgen.CreateAddressParams{
		UserID:        userID,
		Label:         f.Label,
		RecipientName: f.RecipientName,
		Phone:         f.Phone,
		Province:      f.Province,
		City:          f.City,
		District:      f.District,
		Village:       f.Village,
		PostalCode:    f.PostalCode,
		StreetAddress: f.StreetAddress,
		Latitude:      f.Latitude,
		Longitude:     f.Longitude,
		MapsUrl:       nullable(f.MapsURL),
		IsPrimary:     primary,
	})
	if err != nil {
		return Address{}, err
	}
	return fromRow(row), nil
}

func (r *Repository) Update(ctx context.Context, userID, id int64, f fields, primary bool) (Address, error) {
	row, err := r.q.UpdateAddress(ctx, dbgen.UpdateAddressParams{
		ID:            id,
		UserID:        userID,
		Label:         f.Label,
		RecipientName: f.RecipientName,
		Phone:         f.Phone,
		Province:      f.Province,
		City:          f.City,
		District:      f.District,
		Village:       f.Village,
		PostalCode:    f.PostalCode,
		StreetAddress: f.StreetAddress,
		Latitude:      f.Latitude,
		Longitude:     f.Longitude,
		MapsUrl:       nullable(f.MapsURL),
		IsPrimary:     primary,
	})
	if database.IsNoRows(err) {
		return Address{}, ErrNotFound
	}
	if err != nil {
		return Address{}, err
	}
	return fromRow(row), nil
}

func (r *Repository) ClearPrimary(ctx context.Context, userID int64) error {
	return r.q.ClearPrimaryAddress(ctx, userID)
}

func (r *Repository) MarkPrimary(ctx context.Context, userID, id int64) error {
	n, err := r.q.MarkPrimaryAddress(ctx, dbgen.MarkPrimaryAddressParams{ID: id, UserID: userID})
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// Delete removes the address and reports whether it was the primary one.
func (r *Repository) Delete(ctx context.Context, userID, id int64) (wasPrimary bool, err error) {
	wasPrimary, err = r.q.DeleteAddress(ctx, dbgen.DeleteAddressParams{ID: id, UserID: userID})
	if database.IsNoRows(err) {
		return false, ErrNotFound
	}
	return wasPrimary, err
}

func (r *Repository) PromoteNewest(ctx context.Context, userID int64) error {
	return r.q.PromoteNewestAddress(ctx, userID)
}

func fromRow(row dbgen.UserAddress) Address {
	a := Address{
		ID:            row.ID,
		UserID:        row.UserID,
		Label:         row.Label,
		RecipientName: row.RecipientName,
		Phone:         row.Phone,
		Province:      row.Province,
		City:          row.City,
		District:      row.District,
		Village:       row.Village,
		PostalCode:    row.PostalCode,
		StreetAddress: row.StreetAddress,
		Latitude:      row.Latitude,
		Longitude:     row.Longitude,
		IsPrimary:     row.IsPrimary,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
	if row.MapsUrl != nil {
		a.MapsURL = *row.MapsUrl
	}
	return a
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
