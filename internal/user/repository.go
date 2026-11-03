package user

import (
	"context"
	"errors"

	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
)

var ErrNotFound = errors.New("user: not found")

type Repository struct {
	q *dbgen.Queries
}

func NewRepository(db dbgen.DBTX) *Repository { return &Repository{q: dbgen.New(db)} }

func (r *Repository) GetAccount(ctx context.Context, userID int64) (Account, error) {
	row, err := r.q.GetAccount(ctx, userID)
	if database.IsNoRows(err) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, err
	}
	a := Account{
		UserID:    row.ID,
		Email:     row.Email,
		CreatedAt: row.CreatedAt,
		Profile:   Profile{FirstName: row.FirstName, LastName: row.LastName},
	}
	if row.Phone != nil {
		a.Profile.Phone = *row.Phone
	}
	if row.Gender != nil {
		a.Profile.Gender = Gender(*row.Gender)
	}
	return a, nil
}

func (r *Repository) SaveProfile(ctx context.Context, userID int64, p Profile) error {
	return r.q.UpsertProfile(ctx, dbgen.UpsertProfileParams{
		UserID:    userID,
		FirstName: p.FirstName,
		LastName:  p.LastName,
		Phone:     nullable(p.Phone),
		Gender:    nullable(string(p.Gender)),
	})
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
