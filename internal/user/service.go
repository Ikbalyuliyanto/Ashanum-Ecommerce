package user

import (
	"context"

	"ecommerce/internal/validate"
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

func (s *Service) GetAccount(ctx context.Context, userID int64) (Account, error) {
	return s.repo.GetAccount(ctx, userID)
}

// UpdateProfile validates and saves the profile of userID. Invalid input
// returns validate.Errors keyed by first_name, last_name, phone, gender.
func (s *Service) UpdateProfile(ctx context.Context, userID int64, in ProfileInput) (Profile, error) {
	p, err := normalizeProfile(in)
	if err != nil {
		return Profile{}, err
	}
	if err := s.repo.SaveProfile(ctx, userID, p); err != nil {
		return Profile{}, err
	}
	return p, nil
}

func normalizeProfile(in ProfileInput) (Profile, error) {
	errs := validate.Errors{}
	p := Profile{
		FirstName: validate.Text(in.FirstName),
		LastName:  validate.Text(in.LastName),
	}
	validate.Name(errs, "first_name", p.FirstName, "Nama depan", true)
	validate.Name(errs, "last_name", p.LastName, "Nama belakang", false)

	if raw := validate.Text(in.Phone); raw != "" {
		phone, ok := validate.Phone(raw)
		if !ok {
			errs.Add("phone", "Masukkan nomor HP Indonesia yang valid, contoh 081234567890.")
		}
		p.Phone = phone
	}

	switch g := Gender(in.Gender); g {
	case "", GenderMale, GenderFemale:
		p.Gender = g
	default:
		errs.Add("gender", "Pilih jenis kelamin yang tersedia.")
	}
	return p, errs.Err()
}
