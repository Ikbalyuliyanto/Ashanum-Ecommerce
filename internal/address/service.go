package address

import "context"

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// List returns the user's addresses, primary first, then newest first.
func (s *Service) List(ctx context.Context, userID int64) ([]Address, error) {
	return s.repo.List(ctx, userID)
}

// Get returns one address of userID; another user's address is ErrNotFound.
func (s *Service) Get(ctx context.Context, userID, id int64) (Address, error) {
	return s.repo.Get(ctx, userID, id)
}

// Create adds an address. The first address always becomes primary; a later
// one does when in.MakePrimary is set.
func (s *Service) Create(ctx context.Context, userID int64, in Input) (Address, error) {
	f, err := normalize(in)
	if err != nil {
		return Address{}, err
	}

	var out Address
	err = s.repo.InTx(ctx, func(tx *Repository) error {
		if err := tx.LockUser(ctx, userID); err != nil {
			return err
		}
		n, err := tx.Count(ctx, userID)
		if err != nil {
			return err
		}
		if n >= MaxPerUser {
			return ErrLimitReached
		}
		primary := in.MakePrimary || n == 0
		if primary {
			if err := tx.ClearPrimary(ctx, userID); err != nil {
				return err
			}
		}
		out, err = tx.Create(ctx, userID, f, primary)
		return err
	})
	return out, err
}

// Update changes an address. It can become primary but never stops being
// primary here: choose another address as primary instead.
func (s *Service) Update(ctx context.Context, userID, id int64, in Input) (Address, error) {
	f, err := normalize(in)
	if err != nil {
		return Address{}, err
	}

	var out Address
	err = s.repo.InTx(ctx, func(tx *Repository) error {
		if err := tx.LockUser(ctx, userID); err != nil {
			return err
		}
		cur, err := tx.Get(ctx, userID, id)
		if err != nil {
			return err
		}
		primary := cur.IsPrimary || in.MakePrimary
		if primary && !cur.IsPrimary {
			if err := tx.ClearPrimary(ctx, userID); err != nil {
				return err
			}
		}
		out, err = tx.Update(ctx, userID, id, f, primary)
		return err
	})
	return out, err
}

// SetPrimary makes id the only primary address of userID.
func (s *Service) SetPrimary(ctx context.Context, userID, id int64) error {
	return s.repo.InTx(ctx, func(tx *Repository) error {
		if err := tx.LockUser(ctx, userID); err != nil {
			return err
		}
		cur, err := tx.Get(ctx, userID, id)
		if err != nil || cur.IsPrimary {
			return err
		}
		if err := tx.ClearPrimary(ctx, userID); err != nil {
			return err
		}
		return tx.MarkPrimary(ctx, userID, id)
	})
}

// Delete removes an address. Deleting the primary one promotes the newest
// remaining address, so the user always keeps a primary address.
func (s *Service) Delete(ctx context.Context, userID, id int64) error {
	return s.repo.InTx(ctx, func(tx *Repository) error {
		if err := tx.LockUser(ctx, userID); err != nil {
			return err
		}
		wasPrimary, err := tx.Delete(ctx, userID, id)
		if err != nil || !wasPrimary {
			return err
		}
		return tx.PromoteNewest(ctx, userID)
	})
}
