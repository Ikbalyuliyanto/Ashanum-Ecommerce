package shipping

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
)

// Options configure the Service (from SHIP_FROM_*, SHIPPING_*).
type Options struct {
	Origin   Origin
	Couriers []string
	// RateTTL is how long provider rates are reused for the same route and
	// weight (in memory, 5–15 minutes).
	RateTTL time.Duration
	// QuoteTTL is how long a quote shown at checkout stays valid for
	// placing the order.
	QuoteTTL time.Duration
	Log      *slog.Logger
	Now      func() time.Time
}

const (
	defaultRateTTL  = 10 * time.Minute
	defaultQuoteTTL = 30 * time.Minute
	areaQueryTTL    = time.Hour
	maxCacheEntries = 512
	maxAreaResults  = 20
)

type Service struct {
	pool     *pgxpool.Pool
	q        *dbgen.Queries
	provider Provider // nil: shipping not configured
	opts     Options

	rates *ttlCache[[]Rate]
	areas *ttlCache[[]Area]
}

func NewService(pool *pgxpool.Pool, provider Provider, opts Options) *Service {
	if opts.RateTTL <= 0 {
		opts.RateTTL = defaultRateTTL
	}
	if opts.QuoteTTL <= 0 {
		opts.QuoteTTL = defaultQuoteTTL
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{pool: pool, q: dbgen.New(pool), provider: provider, opts: opts,
		rates: newTTLCache[[]Rate](), areas: newTTLCache[[]Area]()}
}

// Enabled reports whether a provider is configured.
func (s *Service) Enabled() bool { return s.provider != nil }

// ProviderName is the configured provider ("" when none).
func (s *Service) ProviderName() string {
	if s.provider == nil {
		return ""
	}
	return s.provider.Name()
}

// ---------------------------------------------------------------- areas

// SearchAreas finds delivery areas (kecamatan, city or postal code) and
// caches them, so an address can save one of them. Repeated queries are
// answered from memory for an hour (the provider's area search has a
// monthly quota).
func (s *Service) SearchAreas(ctx context.Context, query string) ([]Area, error) {
	query = strings.Join(strings.Fields(query), " ")
	if utf8.RuneCountInString(query) < 3 || utf8.RuneCountInString(query) > 60 {
		return nil, ErrInvalidQuery
	}
	if s.provider == nil {
		return nil, ErrNotConfigured
	}
	key := s.provider.Name() + "|" + strings.ToLower(query)
	if areas, ok := s.areas.get(key, s.opts.Now()); ok {
		return areas, nil
	}
	areas, err := s.provider.SearchAreas(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(areas) > maxAreaResults {
		areas = areas[:maxAreaResults]
	}
	if len(areas) > 0 {
		p := dbgen.UpsertShippingAreasParams{Provider: s.provider.Name()}
		for _, a := range areas {
			p.Ids = append(p.Ids, a.ID)
			p.Labels = append(p.Labels, a.Label)
			p.Provinces = append(p.Provinces, a.Province)
			p.Cities = append(p.Cities, a.City)
			p.Districts = append(p.Districts, a.District)
			p.PostalCodes = append(p.PostalCodes, a.PostalCode)
			has := a.Latitude != nil && a.Longitude != nil
			p.HasCoordinates = append(p.HasCoordinates, has)
			lat, lng := 0.0, 0.0
			if has {
				lat, lng = *a.Latitude, *a.Longitude
			}
			p.Latitudes = append(p.Latitudes, lat)
			p.Longitudes = append(p.Longitudes, lng)
		}
		if err := s.q.UpsertShippingAreas(ctx, p); err != nil {
			return nil, err
		}
	}
	s.areas.put(key, areas, s.opts.Now().Add(areaQueryTTL))
	return areas, nil
}

// Area returns a cached area of the configured provider: only areas found
// through SearchAreas can be saved on an address (an id from another
// provider or an invented one is ErrAreaNotFound).
func (s *Service) Area(ctx context.Context, id string) (Area, error) {
	if s.provider == nil {
		return Area{}, ErrNotConfigured
	}
	if id == "" || len(id) > 100 {
		return Area{}, ErrAreaNotFound
	}
	a, err := s.q.GetShippingArea(ctx, dbgen.GetShippingAreaParams{Provider: s.provider.Name(), ProviderAreaID: id})
	if database.IsNoRows(err) {
		return Area{}, ErrAreaNotFound
	}
	if err != nil {
		return Area{}, err
	}
	return Area{Provider: a.Provider, ID: a.ProviderAreaID, Label: a.Label, Province: a.Province, City: a.City,
		District: a.District, PostalCode: a.PostalCode, Latitude: a.Latitude, Longitude: a.Longitude}, nil
}

// ---------------------------------------------------------------- quotes

// Destination is where a package goes: the address's saved area.
type Destination struct {
	Provider string // the provider of the area ("" = none saved)
	AreaID   string
	Label    string
	Postal   string
	Lat      *float64
	Lng      *float64
}

func (d Destination) location() Location {
	return Location{AreaID: d.AreaID, PostalCode: d.Postal, Latitude: d.Lat, Longitude: d.Lng}
}

// Quotes returns the rates for sending pkg to dest as quotes of userID,
// cheapest first. Provider rates are reused for RateTTL; the quotes are
// stored (one statement) so the order can accept only their ids.
func (s *Service) Quotes(ctx context.Context, userID int64, dest Destination, pkg Package) ([]Quote, error) {
	if s.provider == nil {
		return nil, ErrNotConfigured
	}
	if dest.AreaID == "" || dest.Provider != s.provider.Name() {
		return nil, ErrAreaNotFound
	}
	if !pkg.Valid() {
		return nil, ErrPackageInvalid
	}
	rates, err := s.cachedRates(ctx, dest, pkg)
	if err != nil {
		return nil, err
	}

	now := s.opts.Now()
	originKey, destKey, pkgHash := s.originKey(), dest.location().key(), packageHash(pkg)
	p := dbgen.UpsertShippingQuotesParams{UserID: userID, Provider: s.provider.Name(), OriginKey: originKey,
		DestinationKey: destKey, PackageHash: pkgHash, WeightGrams: int32(pkg.WeightGrams()), ExpiresAt: now.Add(s.opts.QuoteTTL)}
	for _, r := range rates {
		p.QuoteKeys = append(p.QuoteKeys, quoteKey(s.provider.Name(), originKey, destKey, pkgHash, r))
		p.CourierCodes = append(p.CourierCodes, r.CourierCode)
		p.CourierNames = append(p.CourierNames, r.CourierName)
		p.ServiceCodes = append(p.ServiceCodes, r.ServiceCode)
		p.ServiceNames = append(p.ServiceNames, r.ServiceName)
		p.Descriptions = append(p.Descriptions, r.Description)
		p.EstimatedDays = append(p.EstimatedDays, r.EstimatedDays)
		p.Prices = append(p.Prices, r.Price)
	}
	rows, err := s.q.UpsertShippingQuotes(ctx, p)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]string, len(rows))
	for _, row := range rows {
		ids[row.QuoteKey] = row.ID
	}
	quotes := make([]Quote, 0, len(rates))
	for i, r := range rates {
		if id := ids[p.QuoteKeys[i]]; id != "" {
			quotes = append(quotes, Quote{ID: id, Rate: r, ExpiresAt: p.ExpiresAt})
		}
	}
	return quotes, nil
}

// cachedRates asks the provider, or reuses its answer for the same route,
// weight and dimensions within RateTTL.
func (s *Service) cachedRates(ctx context.Context, dest Destination, pkg Package) ([]Rate, error) {
	key := s.provider.Name() + "|" + s.originKey() + "|" + dest.location().key() + "|" + packageShape(pkg) + "|" +
		strings.Join(s.opts.Couriers, ",")
	if rates, ok := s.rates.get(key, s.opts.Now()); ok {
		return rates, nil
	}
	rates, err := s.provider.Rates(ctx, RateRequest{Origin: s.opts.Origin.Location(), Destination: dest.location(),
		Package: pkg, Couriers: s.opts.Couriers})
	if err != nil {
		if !errors.Is(err, ErrNoCourier) {
			s.opts.Log.Warn("shipping rates failed", "provider", s.provider.Name(), "kind", errorKind(err), "error", err)
		}
		return nil, err
	}
	rates = dedupe(rates)
	sort.SliceStable(rates, func(i, j int) bool { return rates[i].Price < rates[j].Price })
	s.rates.put(key, rates, s.opts.Now().Add(s.opts.RateTTL))
	return rates, nil
}

// dedupe keeps the first rate per courier service.
func dedupe(rates []Rate) []Rate {
	seen := map[string]bool{}
	out := rates[:0:0]
	for _, r := range rates {
		if !seen[r.Key()] {
			seen[r.Key()] = true
			out = append(out, r)
		}
	}
	return out
}

// UsedQuote is a validated quote, ready to be copied into an order.
type UsedQuote struct {
	ID          string
	Provider    string
	Rate        Rate
	WeightGrams int
	Origin      Origin
}

// UseQuote validates a quote inside the order transaction (q belongs to
// it): the quote must belong to userID, be unused, unexpired, and match the
// current origin, destination and package. Anything else is
// ErrQuoteChanged: the buyer must look at the shipping cost again. The row
// stays locked until the transaction ends; the order statement marks it
// used.
func (s *Service) UseQuote(ctx context.Context, q *dbgen.Queries, userID int64, id string, dest Destination, pkg Package) (UsedQuote, error) {
	if s.provider == nil {
		return UsedQuote{}, ErrNotConfigured
	}
	if !pkg.Valid() {
		return UsedQuote{}, ErrPackageInvalid
	}
	if id == "" || len(id) > 64 {
		return UsedQuote{}, ErrQuoteChanged
	}
	row, err := q.LockShippingQuote(ctx, dbgen.LockShippingQuoteParams{ID: id, UserID: userID})
	if database.IsNoRows(err) {
		return UsedQuote{}, ErrQuoteChanged // unknown, used, or another user's
	}
	if err != nil {
		return UsedQuote{}, err
	}
	switch {
	case !s.opts.Now().Before(row.ExpiresAt),
		row.Provider != s.provider.Name(),
		dest.Provider != s.provider.Name(),
		row.OriginKey != s.originKey(),
		row.DestinationKey != dest.location().key(),
		row.PackageHash != packageHash(pkg),
		int(row.WeightGrams) != pkg.WeightGrams(),
		row.Price <= 0:
		return UsedQuote{}, ErrQuoteChanged
	}
	return UsedQuote{ID: row.ID, Provider: row.Provider, WeightGrams: int(row.WeightGrams), Origin: s.opts.Origin,
		Rate: Rate{Provider: row.Provider, CourierCode: row.CourierCode, CourierName: row.CourierName,
			ServiceCode: row.ServiceCode, ServiceName: row.ServiceName, Description: row.Description,
			Price: row.Price, EstimatedDays: row.EstimatedDays}}, nil
}

// CleanupQuotes deletes unused quotes that expired more than a day ago.
func (s *Service) CleanupQuotes(ctx context.Context) (int64, error) {
	before := s.opts.Now().Add(-24 * time.Hour)
	return s.q.DeleteStaleShippingQuotes(ctx, before)
}

func (s *Service) originKey() string { return s.opts.Origin.Location().key() }

// packageHash identifies the package: which variants, how many, and their
// weights and dimensions. Any change invalidates a quote.
func packageHash(p Package) string {
	parts := make([]string, len(p.Items))
	for i, it := range p.Items {
		parts[i] = fmt.Sprintf("%d:%d:%d:%dx%dx%d", it.VariantID, it.Quantity, it.WeightGrams, it.LengthCM, it.WidthCM, it.HeightCM)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(sum[:16])
}

// packageShape is what the rate depends on (weight and dimensions, not
// which products): two carts of the same weight share cached rates.
func packageShape(p Package) string {
	parts := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		if it.LengthCM > 0 || it.WidthCM > 0 || it.HeightCM > 0 {
			parts = append(parts, fmt.Sprintf("%dx%dx%d*%d", it.LengthCM, it.WidthCM, it.HeightCM, it.Quantity))
		}
	}
	sort.Strings(parts)
	return fmt.Sprintf("%dg[%s]", p.WeightGrams(), strings.Join(parts, ","))
}

func quoteKey(provider, origin, dest, pkg string, r Rate) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{provider, origin, dest, pkg, r.CourierCode, r.ServiceCode}, "|")))
	return hex.EncodeToString(sum[:16])
}

// ---------------------------------------------------------------- cache

// ttlCache is a small in-memory cache with expiry (one process, no Redis).
// When full it drops expired entries, and everything if still full.
type ttlCache[T any] struct {
	mu      sync.Mutex
	entries map[string]ttlEntry[T]
}

type ttlEntry[T any] struct {
	value   T
	expires time.Time
}

func newTTLCache[T any]() *ttlCache[T] { return &ttlCache[T]{entries: map[string]ttlEntry[T]{}} }

func (c *ttlCache[T]) get(key string, now time.Time) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !now.Before(e.expires) {
		var zero T
		return zero, false
	}
	return e.value, true
}

func (c *ttlCache[T]) put(key string, v T, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxCacheEntries {
		now := time.Now()
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= maxCacheEntries {
			clear(c.entries)
		}
	}
	c.entries[key] = ttlEntry[T]{value: v, expires: expires}
}

// errorKind names a provider error for logs without its details.
func errorKind(err error) string {
	switch {
	case errors.Is(err, ErrRejected):
		return "rejected"
	case errors.Is(err, ErrNoCourier):
		return "no courier"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "unavailable"
}
