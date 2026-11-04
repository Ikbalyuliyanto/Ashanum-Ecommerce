// Package report is the admin dashboard and the basic reports: read-only,
// one aggregate SQL statement per block, never a query per row.
//
// Definitions (shown on the report page too):
//
//   - Revenue ("Penjualan"): total of orders paid in the period (paid_at,
//     set only from a provider-confirmed payment) that are not cancelled.
//     WAITING_PAYMENT orders have no paid_at and never count; carts never
//     count. A paid order cancelled later is reported apart with its refund.
//   - Order status breakdown: orders created in the period, by status.
//   - Top products: units of orders counted as revenue.
//   - Refunds: refunds recorded as paid out (processed_at) in the period.
//
// Days are Western Indonesia Time (WIB, UTC+7, no daylight saving).
package report

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/database/dbgen"
	"ecommerce/internal/inventory"
	"ecommerce/internal/order"
)

// WIB is the shop's time zone for day boundaries.
var WIB = time.FixedZone("WIB", 7*60*60)

// Dashboard is the operational summary on /admin.
type Dashboard struct {
	NewToday            int
	WaitingPayment      int
	WaitingConfirmation int
	Processing          int
	Shipped             int
	PaidToday           int
	SalesToday          int64
	PaidWeek            int
	SalesWeek           int64
	ReturnsRequested    int
	ReturnsInProgress   int
	RefundsOpen         int
	RefundsOpenAmount   int64
	OutOfStock          int
	LowStock            int
	LowStockThreshold   int
	At                  time.Time
}

// Range is a report period [From, To) in WIB days.
type Range struct {
	Key      string // today | 7d | 30d | custom
	From     time.Time
	To       time.Time // exclusive
	FromDate string    // YYYY-MM-DD, inclusive
	ToDate   string    // YYYY-MM-DD, inclusive
}

// Days is the number of days in the range.
func (r Range) Days() int { return int(r.To.Sub(r.From).Hours()/24 + 0.5) }

// MaxRangeDays bounds a custom range.
const MaxRangeDays = 366

var ErrBadRange = errors.New("report: invalid date range")

// ParseRange reads the period filter. Unknown keys fall back to 7 days; a
// custom range needs from ≤ to (YYYY-MM-DD) within MaxRangeDays.
func ParseRange(key, from, to string, now time.Time) (Range, error) {
	today := day(now.In(WIB))
	r := Range{Key: key}
	switch key {
	case "today":
		r.From, r.To = today, today.AddDate(0, 0, 1)
	case "30d":
		r.From, r.To = today.AddDate(0, 0, -29), today.AddDate(0, 0, 1)
	case "custom":
		f, err1 := time.ParseInLocation(time.DateOnly, from, WIB)
		t, err2 := time.ParseInLocation(time.DateOnly, to, WIB)
		if err1 != nil || err2 != nil || t.Before(f) || t.Sub(f) > (MaxRangeDays-1)*24*time.Hour {
			r7, _ := ParseRange("7d", "", "", now)
			return r7, ErrBadRange
		}
		r.From, r.To = f, t.AddDate(0, 0, 1)
	default:
		r.Key = "7d"
		r.From, r.To = today.AddDate(0, 0, -6), today.AddDate(0, 0, 1)
	}
	r.FromDate, r.ToDate = r.From.Format(time.DateOnly), r.To.AddDate(0, 0, -1).Format(time.DateOnly)
	return r, nil
}

func day(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, WIB) }

// Sales is the revenue block.
type Sales struct {
	PaidOrders      int
	Revenue         int64
	ItemsValue      int64
	Units           int
	CancelledOrders int // paid in the period, cancelled later
	CancelledAmount int64
}

// AverageOrder is revenue per paid order (0 when none).
func (s Sales) AverageOrder() int64 {
	if s.PaidOrders == 0 {
		return 0
	}
	return s.Revenue / int64(s.PaidOrders)
}

type StatusRow struct {
	Status order.Status
	Orders int
	Amount int64
}

type TopProduct struct {
	ProductID int64 // 0 once deleted from the catalog
	Name      string
	Units     int
	Revenue   int64
	Orders    int
}

type Returns struct {
	Returns               int
	RefundReturns         int
	ExchangeReturns       int
	RefundedReturns       int64 // paid out for returns in the period
	RefundedCancellations int64 // paid out for cancelled paid orders
	RefundsOpen           int64 // still to pay, any period
}

// Report is the /admin/reports page.
type Report struct {
	Range       Range
	Sales       Sales
	Statuses    []StatusRow
	OrdersTotal int
	TopProducts []TopProduct
	Returns     Returns
	LowStock    []inventory.Item
	Threshold   int
}

type Service struct {
	q         *dbgen.Queries
	inventory *inventory.Service
	now       func() time.Time
}

func NewService(pool *pgxpool.Pool, inv *inventory.Service) *Service {
	return &Service{q: dbgen.New(pool), inventory: inv, now: time.Now}
}

// Dashboard returns the operational counters (one query).
func (s *Service) Dashboard(ctx context.Context) (Dashboard, error) {
	now := s.now()
	today := day(now.In(WIB))
	week := today.AddDate(0, 0, -6)
	threshold := s.inventory.LowStockThreshold()
	row, err := s.q.Dashboard(ctx, dbgen.DashboardParams{DayStart: today, WeekStart: &week, Threshold: int32(threshold)})
	if err != nil {
		return Dashboard{}, err
	}
	return Dashboard{NewToday: int(row.NewToday), WaitingPayment: int(row.WaitingPayment),
		WaitingConfirmation: int(row.WaitingConfirmation), Processing: int(row.Processing), Shipped: int(row.Shipped),
		PaidToday: int(row.PaidToday), SalesToday: row.SalesToday, PaidWeek: int(row.PaidWeek), SalesWeek: row.SalesWeek,
		ReturnsRequested: int(row.ReturnsRequested), ReturnsInProgress: int(row.ReturnsInProgress),
		RefundsOpen: int(row.RefundsOpen), RefundsOpenAmount: row.RefundsOpenAmount,
		OutOfStock: int(row.OutOfStock), LowStock: int(row.LowStock), LowStockThreshold: threshold, At: now}, nil
}

// TopLimit is the number of top products shown.
const TopLimit = 10

// Build returns the report for a period (five aggregate queries).
func (s *Service) Build(ctx context.Context, r Range) (Report, error) {
	rep := Report{Range: r, Threshold: s.inventory.LowStockThreshold()}
	from, to := r.From, r.To
	sales, err := s.q.SalesSummary(ctx, dbgen.SalesSummaryParams{FromTime: &from, ToTime: &to})
	if err != nil {
		return rep, err
	}
	rep.Sales = Sales{PaidOrders: int(sales.PaidOrders), Revenue: sales.Revenue, ItemsValue: sales.ItemsValue,
		Units: int(sales.Units), CancelledOrders: int(sales.CancelledOrders), CancelledAmount: sales.CancelledAmount}

	statuses, err := s.q.OrderStatusBreakdown(ctx, dbgen.OrderStatusBreakdownParams{FromTime: from, ToTime: to})
	if err != nil {
		return rep, err
	}
	counts := map[order.Status]StatusRow{}
	for _, st := range statuses {
		counts[order.Status(st.Status)] = StatusRow{Status: order.Status(st.Status), Orders: int(st.Orders), Amount: st.Amount}
		rep.OrdersTotal += int(st.Orders)
	}
	for _, st := range []order.Status{order.StatusWaitingPayment, order.StatusWaitingConfirmation, order.StatusProcessing,
		order.StatusShipped, order.StatusCompleted, order.StatusCancelled} {
		row := counts[st]
		row.Status = st
		rep.Statuses = append(rep.Statuses, row)
	}

	top, err := s.q.TopProducts(ctx, dbgen.TopProductsParams{FromTime: &from, ToTime: &to, RowLimit: TopLimit})
	if err != nil {
		return rep, err
	}
	for _, t := range top {
		p := TopProduct{Name: t.ProductName, Units: int(t.Units), Revenue: t.Revenue, Orders: int(t.Orders)}
		if t.ProductID != nil {
			p.ProductID = *t.ProductID
		}
		rep.TopProducts = append(rep.TopProducts, p)
	}

	ret, err := s.q.ReturnSummary(ctx, dbgen.ReturnSummaryParams{FromTime: from, ToTime: to})
	if err != nil {
		return rep, err
	}
	rep.Returns = Returns{Returns: int(ret.Returns), RefundReturns: int(ret.RefundReturns), ExchangeReturns: int(ret.ExchangeReturns),
		RefundedReturns: ret.RefundedReturns, RefundedCancellations: ret.RefundedCancellations, RefundsOpen: ret.RefundsOpen}

	low, err := s.inventory.List(ctx, inventory.Filter{Status: "low", PageSize: 10})
	if err != nil {
		return rep, err
	}
	rep.LowStock = low.Items
	return rep, nil
}

// Refunded is everything paid back in the period (returns + cancellations).
func (r Returns) Refunded() int64 { return r.RefundedReturns + r.RefundedCancellations }
