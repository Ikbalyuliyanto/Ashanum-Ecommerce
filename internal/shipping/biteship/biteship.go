// Package biteship implements shipping.Provider for Biteship
// (https://biteship.com/id/docs/api) with net/http only: area search
// (GET /v1/maps/areas), rates (POST /v1/rates/couriers), orders (POST
// /v1/orders, GET /v1/orders/:id, POST /v1/orders/:id/cancel), tracking
// (GET /v1/trackings/:id) and the order webhooks.
//
// Keys are environment specific: biteship_test.* (testing mode, no real
// pickup) and biteship_live.*. The live environment is refused unless the
// configuration explicitly enables live shipping. The key is sent only in
// the Authorization header and never logged or returned in errors.
package biteship

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ecommerce/internal/shipping"
)

const (
	EnvTest = "test"
	EnvLive = "live"

	DefaultBaseURL = "https://api.biteship.com"

	providerName = "biteship"

	// maxResponse bounds every response body read (rates for many couriers
	// are a few KiB).
	maxResponse = 1 << 20

	// Biteship error code for a reference_id used before; its details name
	// the existing order.
	codeDuplicateReference = 40002060
	// Rates: no courier serves the requested route.
	codeNoCourier = 40001010
)

type Config struct {
	Env    string // EnvTest or EnvLive
	APIKey string // backend only: never rendered, logged or stored
	// LiveEnabled must be true to use EnvLive (SHIPPING_LIVE_ENABLED).
	LiveEnabled bool
	// BaseURL replaces https://api.biteship.com (local fakes and tests).
	BaseURL string
	// WebhookHeader / WebhookSecret are the "Headers Signature Key" and
	// "Secret" configured for the webhook in the Biteship dashboard: every
	// notification carries header WebhookHeader with value WebhookSecret.
	WebhookHeader string
	WebhookSecret string
	HTTPClient    *http.Client
}

type Client struct {
	key           string
	baseURL       string
	webhookHeader string
	webhookSecret string
	http          *http.Client
}

var _ shipping.Provider = (*Client)(nil)

func New(cfg Config) (*Client, error) {
	switch cfg.Env {
	case EnvTest:
		if !strings.HasPrefix(cfg.APIKey, "biteship_test.") {
			return nil, errors.New("biteship: BITESHIP_ENV=test needs a testing key (biteship_test.…)")
		}
	case EnvLive:
		if !cfg.LiveEnabled {
			return nil, errors.New("biteship: live environment refused (SHIPPING_LIVE_ENABLED is not true)")
		}
		if !strings.HasPrefix(cfg.APIKey, "biteship_live.") {
			return nil, errors.New("biteship: BITESHIP_ENV=live needs a live key (biteship_live.…)")
		}
	default:
		return nil, fmt.Errorf("biteship: unknown environment %q (want test or live)", cfg.Env)
	}
	c := &Client{key: cfg.APIKey, baseURL: DefaultBaseURL, webhookHeader: cfg.WebhookHeader,
		webhookSecret: cfg.WebhookSecret, http: cfg.HTTPClient}
	if cfg.BaseURL != "" {
		c.baseURL = strings.TrimRight(cfg.BaseURL, "/")
	}
	if c.http == nil {
		c.http = NewHTTPClient()
	}
	return c, nil
}

// NewHTTPClient is a client with a total timeout and a small reused
// connection pool (one provider host).
func NewHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 4
	t.MaxIdleConnsPerHost = 4
	t.IdleConnTimeout = 90 * time.Second
	t.ResponseHeaderTimeout = 10 * time.Second
	return &http.Client{Timeout: 15 * time.Second, Transport: t}
}

func (c *Client) Name() string { return providerName }

// ---------------------------------------------------------------- areas

type areaJSON struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	CountryCode string   `json:"country_code"`
	Level1      string   `json:"administrative_division_level_1_name"`
	Level2      string   `json:"administrative_division_level_2_name"`
	Level3      string   `json:"administrative_division_level_3_name"`
	PostalCode  flexText `json:"postal_code"`
	Latitude    *float64 `json:"latitude"`
	Longitude   *float64 `json:"longitude"`
}

// SearchAreas uses single search: each result is a kecamatan with its
// postal code, ready to save on an address.
func (c *Client) SearchAreas(ctx context.Context, query string) ([]shipping.Area, error) {
	q := url.Values{"countries": {"ID"}, "input": {query}, "type": {"single"}}
	var out struct {
		Areas []areaJSON `json:"areas"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/maps/areas?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	areas := make([]shipping.Area, 0, len(out.Areas))
	for _, a := range out.Areas {
		if a.ID == "" || (a.CountryCode != "" && a.CountryCode != "ID") {
			continue
		}
		area := shipping.Area{Provider: providerName, ID: a.ID, Label: clip(a.Name, 300), Province: clip(a.Level1, 60),
			City: clip(a.Level2, 60), District: clip(a.Level3, 60), PostalCode: string(a.PostalCode)}
		if !postalCode(area.PostalCode) {
			area.PostalCode = ""
		}
		if a.Latitude != nil && a.Longitude != nil {
			area.Latitude, area.Longitude = a.Latitude, a.Longitude
		}
		if area.Label == "" {
			continue
		}
		areas = append(areas, area)
	}
	return areas, nil
}

// ---------------------------------------------------------------- rates

type itemJSON struct {
	Name        string `json:"name"`
	SKU         string `json:"sku,omitempty"`
	Description string `json:"description,omitempty"`
	Value       int64  `json:"value"`
	Weight      int    `json:"weight"`
	Quantity    int    `json:"quantity"`
	Length      int    `json:"length,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
}

func items(p shipping.Package) []itemJSON {
	out := make([]itemJSON, len(p.Items))
	for i, it := range p.Items {
		out[i] = itemJSON{Name: clip(it.Name, 100), SKU: it.SKU, Value: it.Value, Weight: it.WeightGrams, Quantity: it.Quantity,
			Length: it.LengthCM, Width: it.WidthCM, Height: it.HeightCM}
	}
	return out
}

type pricingJSON struct {
	CourierCode        string   `json:"courier_code"`
	CourierName        string   `json:"courier_name"`
	CourierServiceCode string   `json:"courier_service_code"`
	CourierServiceName string   `json:"courier_service_name"`
	Description        string   `json:"description"`
	Duration           string   `json:"duration"`
	DurationRange      flexText `json:"shipment_duration_range"`
	DurationUnit       string   `json:"shipment_duration_unit"`
	Price              int64    `json:"price"`
}

// Rates asks for the given couriers. Locations use the area id when known,
// otherwise the postal code.
func (c *Client) Rates(ctx context.Context, req shipping.RateRequest) ([]shipping.Rate, error) {
	body := map[string]any{
		"couriers": strings.Join(req.Couriers, ","),
		"items":    items(req.Package),
	}
	location(body, "origin", req.Origin)
	location(body, "destination", req.Destination)
	var out struct {
		Pricing []pricingJSON `json:"pricing"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/rates/couriers", body, &out); err != nil {
		return nil, err
	}
	rates := make([]shipping.Rate, 0, len(out.Pricing))
	for _, p := range out.Pricing {
		if p.CourierCode == "" || p.CourierServiceCode == "" || p.Price <= 0 {
			continue // incomplete offer: never show a 0 or nameless rate
		}
		rates = append(rates, shipping.Rate{Provider: providerName, CourierCode: clip(p.CourierCode, 40),
			CourierName: clip(nonEmpty(p.CourierName, strings.ToUpper(p.CourierCode)), 60),
			ServiceCode: clip(p.CourierServiceCode, 60), ServiceName: clip(nonEmpty(p.CourierServiceName, p.CourierServiceCode), 60),
			Description: clip(p.Description, 200), Price: p.Price,
			EstimatedDays: estimatedDays(string(p.DurationRange), p.DurationUnit, p.Duration)})
	}
	if len(rates) == 0 {
		return nil, shipping.ErrNoCourier
	}
	return rates, nil
}

func location(body map[string]any, prefix string, l shipping.Location) {
	switch {
	case l.AreaID != "":
		body[prefix+"_area_id"] = l.AreaID
	case l.PostalCode != "":
		if n, err := strconv.Atoi(l.PostalCode); err == nil {
			body[prefix+"_postal_code"] = n
		}
	}
}

// estimatedDays normalizes "2 - 3" + "days" to "2-3"; hours become "0"
// (same day); unknown units give "".
func estimatedDays(rng, unit, duration string) string {
	if rng == "" {
		rng, _, _ = strings.Cut(duration, " day")
	}
	parts := strings.Split(rng, "-")
	nums := make([]string, 0, 2)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if _, err := strconv.Atoi(p); err != nil || len(nums) == 2 {
			return ""
		}
		nums = append(nums, p)
	}
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "hours", "hour", "jam":
		return "0"
	case "days", "day", "hari", "":
	default:
		return ""
	}
	if len(nums) == 2 && nums[0] == nums[1] {
		nums = nums[:1]
	}
	return strings.Join(nums, "-")
}

// ---------------------------------------------------------------- orders

type orderJSON struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	ReferenceID string `json:"reference_id"`
	Courier     struct {
		TrackingID string `json:"tracking_id"`
		WaybillID  string `json:"waybill_id"`
		Link       string `json:"link"`
	} `json:"courier"`
}

func (o orderJSON) booked() shipping.Booked {
	st, _ := MapStatus(o.Status)
	return shipping.Booked{ShipmentID: o.ID, TrackingID: o.Courier.TrackingID, Waybill: clip(o.Courier.WaybillID, 60),
		Status: st, RawStatus: clip(o.Status, 40), TrackingURL: safeLink(o.Courier.Link)}
}

// CreateShipment books a pickup ("now"). reference_id is our booking key:
// Biteship refuses a reused one with code 40002060 and names the existing
// order, which is returned as Existing (retry after a timeout).
func (c *Client) CreateShipment(ctx context.Context, b shipping.Booking) (shipping.Booked, error) {
	body := map[string]any{
		"shipper_contact_name":      clip(b.Origin.Name, 100),
		"shipper_contact_phone":     localPhone(b.Origin.Phone),
		"origin_contact_name":       clip(b.Origin.Name, 100),
		"origin_contact_phone":      localPhone(b.Origin.Phone),
		"origin_address":            clip(b.Origin.Address, 300),
		"destination_contact_name":  clip(b.Destination.Name, 100),
		"destination_contact_phone": localPhone(b.Destination.Phone),
		"destination_address":       clip(b.Destination.Address, 300),
		"courier_company":           b.CourierCode,
		"courier_type":              b.ServiceCode,
		"delivery_type":             "now",
		"reference_id":              b.Reference,
		"order_note":                clip(b.Note, 200),
		"items":                     items(b.Package),
	}
	location(body, "origin", b.Origin.Location())
	if b.Origin.PostalCode != "" {
		if n, err := strconv.Atoi(b.Origin.PostalCode); err == nil {
			body["origin_postal_code"] = n
		}
	}
	if b.Origin.Latitude != nil && b.Origin.Longitude != nil {
		body["origin_coordinate"] = map[string]float64{"latitude": *b.Origin.Latitude, "longitude": *b.Origin.Longitude}
	}
	location(body, "destination", shipping.Location{AreaID: b.Destination.AreaID, PostalCode: b.Destination.PostalCode})
	if b.Destination.PostalCode != "" {
		if n, err := strconv.Atoi(b.Destination.PostalCode); err == nil {
			body["destination_postal_code"] = n
		}
	}
	if b.Destination.Latitude != nil && b.Destination.Longitude != nil {
		body["destination_coordinate"] = map[string]float64{"latitude": *b.Destination.Latitude, "longitude": *b.Destination.Longitude}
	}

	var out orderJSON
	err := c.do(ctx, http.MethodPost, "/v1/orders", body, &out)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Code == codeDuplicateReference && apiErr.ExistingOrderID != "" {
		existing, err := c.order(ctx, apiErr.ExistingOrderID)
		if err != nil {
			return shipping.Booked{}, err
		}
		bk := existing.booked()
		bk.Existing = true
		return bk, nil
	}
	if err != nil {
		return shipping.Booked{}, err
	}
	if out.ID == "" {
		return shipping.Booked{}, fmt.Errorf("%w: order without id", shipping.ErrUnavailable)
	}
	return out.booked(), nil
}

func (c *Client) order(ctx context.Context, id string) (orderJSON, error) {
	var out orderJSON
	if err := c.do(ctx, http.MethodGet, "/v1/orders/"+url.PathEscape(id), nil, &out); err != nil {
		return orderJSON{}, err
	}
	if out.ID == "" {
		return orderJSON{}, fmt.Errorf("%w: order without id", shipping.ErrUnavailable)
	}
	return out, nil
}

// CancelShipment cancels with reason code "others" and our reason text.
func (c *Client) CancelShipment(ctx context.Context, shipmentID, reason string) error {
	body := map[string]any{"cancellation_reason_code": "others", "cancellation_reason": clip(reason, 200)}
	var out struct {
		Status string `json:"status"`
	}
	return c.do(ctx, http.MethodPost, "/v1/orders/"+url.PathEscape(shipmentID)+"/cancel", body, &out)
}

// ---------------------------------------------------------------- tracking

type trackingJSON struct {
	ID        string `json:"id"`
	WaybillID string `json:"waybill_id"`
	Status    string `json:"status"`
	Link      string `json:"link"`
	History   []struct {
		Note      string `json:"note"`
		Status    string `json:"status"`
		UpdatedAt string `json:"updated_at"`
	} `json:"history"`
}

// Tracking reads GET /v1/trackings/:tracking_id. Without a tracking id
// (an adopted retry), the order is read first to find it.
func (c *Client) Tracking(ctx context.Context, shipmentID, trackingID string) (shipping.Tracking, error) {
	if trackingID == "" {
		o, err := c.order(ctx, shipmentID)
		if err != nil {
			return shipping.Tracking{}, err
		}
		if trackingID = o.Courier.TrackingID; trackingID == "" {
			st, _ := MapStatus(o.Status)
			return shipping.Tracking{Status: st, RawStatus: clip(o.Status, 40), Waybill: clip(o.Courier.WaybillID, 60),
				TrackingURL: safeLink(o.Courier.Link)}, nil
		}
	}
	var out trackingJSON
	if err := c.do(ctx, http.MethodGet, "/v1/trackings/"+url.PathEscape(trackingID), nil, &out); err != nil {
		return shipping.Tracking{}, err
	}
	st, _ := MapStatus(out.Status)
	t := shipping.Tracking{Status: st, RawStatus: clip(out.Status, 40), Waybill: clip(out.WaybillID, 60), TrackingURL: safeLink(out.Link)}
	for _, h := range out.History {
		hs, ok := MapStatus(h.Status)
		at, err := time.Parse(time.RFC3339, h.UpdatedAt)
		if !ok || err != nil {
			continue // unknown status or time: not a usable event
		}
		t.Events = append(t.Events, shipping.Event{Status: hs, RawStatus: clip(normalize(h.Status), 40),
			Description: clip(h.Note, 500), Time: at})
	}
	return t, nil
}

// MapStatus maps a Biteship order/tracking status (snake_case or camelCase)
// to the internal status; ok is false for an unknown status.
func MapStatus(raw string) (shipping.Status, bool) {
	switch normalize(raw) {
	case "confirmed", "scheduled", "allocated", "pickingup":
		return shipping.StatusCreated, true
	case "picked":
		return shipping.StatusPickedUp, true
	case "intransit":
		return shipping.StatusInTransit, true
	case "droppingoff":
		return shipping.StatusOutForDelivery, true
	case "delivered":
		return shipping.StatusDelivered, true
	case "onhold", "rejected", "disposed", "returnintransit":
		return shipping.StatusException, true
	case "returned":
		return shipping.StatusReturned, true
	case "cancelled", "canceled", "couriernotfound":
		return shipping.StatusCancelled, true
	}
	return "", false
}

// normalize turns "picking_up", "pickingUp" and "Picking Up" into
// "pickingup".
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- webhook

type webhookJSON struct {
	Event       string `json:"event"`
	OrderID     string `json:"order_id"`
	TrackingID  string `json:"courier_tracking_id"`
	WaybillID   string `json:"courier_waybill_id"`
	CourierLink string `json:"courier_link"`
	Status      string `json:"status"`
}

// ParseWebhook checks the shared header secret (constant time) before
// reading anything. An empty body (Biteship's installation test) is a ping.
func (c *Client) ParseWebhook(header http.Header, body []byte) (shipping.WebhookEvent, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || string(trimmed) == "{}" {
		return shipping.WebhookEvent{Kind: shipping.WebhookPing}, nil
	}
	if c.webhookHeader == "" || c.webhookSecret == "" {
		return shipping.WebhookEvent{}, shipping.ErrInvalidSignature // not configured: accept nothing
	}
	got := header.Get(c.webhookHeader)
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(c.webhookSecret)) != 1 {
		return shipping.WebhookEvent{}, shipping.ErrInvalidSignature
	}
	var w webhookJSON
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&w); err != nil || dec.More() {
		return shipping.WebhookEvent{}, shipping.ErrMalformed
	}
	ev := shipping.WebhookEvent{ShipmentID: w.OrderID, Waybill: clip(w.WaybillID, 60), TrackingURL: safeLink(w.CourierLink),
		RawStatus: clip(normalize(w.Status), 40)}
	switch w.Event {
	case "order.status":
		ev.Kind = shipping.WebhookStatus
		ev.Status, _ = MapStatus(w.Status)
	case "order.waybill_id":
		ev.Kind = shipping.WebhookWaybill
	default:
		ev.Kind = shipping.WebhookOther
	}
	if ev.Kind != shipping.WebhookOther && ev.ShipmentID == "" {
		return shipping.WebhookEvent{}, shipping.ErrMalformed
	}
	return ev, nil
}

// ---------------------------------------------------------------- transport

// apiError is a Biteship error answer (success false).
type apiError struct {
	Status          int
	Code            int
	Message         string
	ExistingOrderID string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("biteship: HTTP %d code %d: %s", e.Status, e.Code, e.Message)
}

// Unwrap classifies the error: 4xx is a refusal, anything else means the
// provider is not usable right now.
func (e *apiError) Unwrap() error {
	if e.Code == codeNoCourier {
		return shipping.ErrNoCourier
	}
	if e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests {
		return shipping.ErrRejected
	}
	return shipping.ErrUnavailable
}

type errorJSON struct {
	Success *bool           `json:"success"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
	Code    int             `json:"code"`
	Details json.RawMessage `json:"details"`
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.key)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The error text may contain the URL, never the key.
		return fmt.Errorf("%w: %s %s: %v", shipping.ErrUnavailable, method, strings.SplitN(path, "?", 2)[0], unwrapURL(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("%w: read: %v", shipping.ErrUnavailable, err)
	}
	if len(raw) > maxResponse {
		return fmt.Errorf("%w: response too large", shipping.ErrUnavailable)
	}

	var e errorJSON
	jsonErr := json.Unmarshal(raw, &e)
	if resp.StatusCode < 200 || resp.StatusCode > 299 || (jsonErr == nil && e.Success != nil && !*e.Success) {
		ae := &apiError{Status: resp.StatusCode, Code: e.Code, Message: clip(nonEmpty(e.Error, e.Message), 200)}
		if ae.Status >= 200 && ae.Status < 300 {
			ae.Status = http.StatusBadRequest // success:false with 200
		}
		if ae.Code == codeDuplicateReference {
			var d struct {
				OrderID string `json:"order_id"`
			}
			if json.Unmarshal(e.Details, &d) == nil {
				ae.ExistingOrderID = d.OrderID
			}
		}
		return ae
	}
	if jsonErr != nil {
		return fmt.Errorf("%w: malformed JSON from %s", shipping.ErrUnavailable, strings.SplitN(path, "?", 2)[0])
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: unexpected JSON from %s", shipping.ErrUnavailable, strings.SplitN(path, "?", 2)[0])
	}
	return nil
}

// unwrapURL drops the *url.Error wrapper (its text repeats the URL).
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// ---------------------------------------------------------------- helpers

// flexText accepts a JSON string or number (postal codes and duration
// ranges come as either).
type flexText string

func (f *flexText) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*f = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexText(strings.TrimSpace(s))
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexText(n.String())
	return nil
}

func postalCode(s string) bool {
	if len(s) != 5 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// localPhone turns +6281… into 081…, the form couriers expect.
func localPhone(p string) string {
	if rest, ok := strings.CutPrefix(p, "+62"); ok {
		return "0" + rest
	}
	return p
}

// safeLink keeps only https links (shown as a "lacak di situs kurir" link).
func safeLink(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme != "https" || u.Host == "" || len(s) > 500 {
		return ""
	}
	return u.String()
}

func nonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return b
}

// clip trims and cuts s to n runes.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
