package api

import (
	"net/http"
	"strconv"

	"ecommerce/internal/address"
	"ecommerce/internal/auth"
	"ecommerce/internal/user"
)

// ---------------------------------------------------------------- auth

type credentialsReq struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	DeviceName string `json:"device_name"`
}

// register creates a CUSTOMER account (auth.Service.Register: same rules
// and bcrypt as the web; a role is never read from the request) and signs
// the app in.
func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req credentialsReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	u, err := h.Auth.Register(r.Context(), auth.RegisterInput{Email: req.Email, Password: req.Password, PasswordConfirm: req.Password})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	pair, err := h.Auth.IssueAPITokens(r.Context(), u.ID, req.DeviceName)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.signedIn(w, r, http.StatusCreated, u.ID, pair)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req credentialsReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	u, pair, err := h.Auth.LoginAPI(r.Context(), req.Email, req.Password, req.DeviceName)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.signedIn(w, r, http.StatusOK, u.ID, pair)
}

func (h *Handler) signedIn(w http.ResponseWriter, r *http.Request, status int, uid int64, pair auth.TokenPair) {
	a, err := h.Users.GetAccount(r.Context(), uid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	t := tokens(pair)
	u := account(a)
	t.User = &u
	w.Header().Set("Cache-Control", "no-store")
	ok(w, status, t)
}

type refreshReq struct {
	RefreshToken string `json:"refresh_token"`
}

// refresh rotates the refresh token: the old pair stops working and a new
// pair is returned. Reusing an old refresh token revokes the device.
func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.RefreshToken == "" {
		h.fail(w, r, invalid(map[string]string{"refresh_token": "Refresh token wajib diisi."}))
		return
	}
	pair, err := h.Auth.RefreshAPI(r.Context(), req.RefreshToken)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ok(w, http.StatusOK, tokens(pair))
}

// logout ends this device's API session: by its refresh token in the body,
// or by the Bearer access token. Web sessions are not affected.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var req refreshReq
	if r.ContentLength != 0 {
		if err := decode(w, r, &req); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	if req.RefreshToken != "" {
		if err := h.Auth.LogoutAPIByRefresh(r.Context(), req.RefreshToken); err != nil {
			h.fail(w, r, err)
			return
		}
		noContent(w)
		return
	}
	token := bearer(r)
	if token == "" {
		failWith(w, http.StatusUnauthorized, "UNAUTHORIZED", "Kirim refresh_token atau Authorization: Bearer.", nil)
		return
	}
	p, err := h.Auth.AuthenticateAPI(r.Context(), token)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.Auth.LogoutAPI(r.Context(), p.FamilyID); err != nil {
		h.fail(w, r, err)
		return
	}
	noContent(w)
}

// logoutAll revokes every API session of the user (all devices).
func (h *Handler) logoutAll(w http.ResponseWriter, r *http.Request) {
	if _, err := h.Auth.LogoutAllAPI(r.Context(), userID(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	noContent(w)
}

// ---------------------------------------------------------------- me

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	a, err := h.Users.GetAccount(r.Context(), userID(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ok(w, http.StatusOK, account(a))
}

type profileReq struct {
	FirstName *string `json:"first_name"`
	LastName  *string `json:"last_name"`
	Phone     *string `json:"phone"`
	Gender    *string `json:"gender"`
}

// updateProfile changes only the fields sent (PATCH); the rest keep their
// current value. Validation is user.Service's.
func (h *Handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	var req profileReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	uid := userID(r)
	a, err := h.Users.GetAccount(r.Context(), uid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	in := user.ProfileInput{FirstName: a.Profile.FirstName, LastName: a.Profile.LastName, Phone: a.Profile.Phone, Gender: string(a.Profile.Gender)}
	set(&in.FirstName, req.FirstName)
	set(&in.LastName, req.LastName)
	set(&in.Phone, req.Phone)
	set(&in.Gender, req.Gender)
	if _, err := h.Users.UpdateProfile(r.Context(), uid, in); err != nil {
		h.fail(w, r, err)
		return
	}
	h.me(w, r)
}

func set(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}

// ---------------------------------------------------------------- addresses

// addressReq is an address body; on PATCH only the fields sent change.
type addressReq struct {
	Label         *string  `json:"label"`
	RecipientName *string  `json:"recipient_name"`
	Phone         *string  `json:"phone"`
	Province      *string  `json:"province"`
	City          *string  `json:"city"`
	District      *string  `json:"district"`
	Village       *string  `json:"village"`
	PostalCode    *string  `json:"postal_code"`
	StreetAddress *string  `json:"street_address"`
	Latitude      *float64 `json:"latitude"`
	Longitude     *float64 `json:"longitude"`
	MapsURL       *string  `json:"maps_url"`
	IsPrimary     *bool    `json:"is_primary"`
	// ShippingAreaID is an id from GET /shipping/areas ("" removes it).
	// The area's province, city, district and postal code are saved.
	ShippingAreaID *string `json:"shipping_area_id"`
}

func (q addressReq) apply(in *address.Input) {
	set(&in.Label, q.Label)
	set(&in.RecipientName, q.RecipientName)
	set(&in.Phone, q.Phone)
	set(&in.Province, q.Province)
	set(&in.City, q.City)
	set(&in.District, q.District)
	set(&in.Village, q.Village)
	set(&in.PostalCode, q.PostalCode)
	set(&in.StreetAddress, q.StreetAddress)
	set(&in.MapsURL, q.MapsURL)
	set(&in.ShippingAreaID, q.ShippingAreaID)
	if q.Latitude != nil {
		in.Latitude = strconv.FormatFloat(*q.Latitude, 'f', -1, 64)
	}
	if q.Longitude != nil {
		in.Longitude = strconv.FormatFloat(*q.Longitude, 'f', -1, 64)
	}
	if q.IsPrimary != nil {
		in.MakePrimary = *q.IsPrimary
	}
}

func (h *Handler) addresses(w http.ResponseWriter, r *http.Request) {
	list, err := h.Addresses.List(r.Context(), userID(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]addressDTO, len(list))
	for i, a := range list {
		out[i] = addressOut(a)
	}
	ok(w, http.StatusOK, out)
}

func (h *Handler) createAddress(w http.ResponseWriter, r *http.Request) {
	var req addressReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	var in address.Input
	req.apply(&in)
	a, err := h.Addresses.Create(r.Context(), userID(r), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", Prefix+"/me/addresses/"+strconv.FormatInt(a.ID, 10))
	ok(w, http.StatusCreated, addressOut(a))
}

func (h *Handler) updateAddress(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req addressReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	uid := userID(r)
	cur, err := h.Addresses.Get(r.Context(), uid, id) // another user's address is ErrNotFound
	if err != nil {
		h.fail(w, r, err)
		return
	}
	in := address.InputFrom(cur)
	req.apply(&in)
	a, err := h.Addresses.Update(r.Context(), uid, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ok(w, http.StatusOK, addressOut(a))
}

func (h *Handler) deleteAddress(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err == nil {
		err = h.Addresses.Delete(r.Context(), userID(r), id)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	noContent(w)
}

func (h *Handler) primaryAddress(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	uid := userID(r)
	if err == nil {
		err = h.Addresses.SetPrimary(r.Context(), uid, id)
	}
	var a address.Address
	if err == nil {
		a, err = h.Addresses.Get(r.Context(), uid, id)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ok(w, http.StatusOK, addressOut(a))
}
