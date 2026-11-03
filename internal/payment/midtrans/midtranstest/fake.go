// Package midtranstest is an in-memory imitation of the Midtrans Snap and
// Core API endpoints the midtrans client uses, for tests without internet.
// It is never used by the application binary.
package midtranstest

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const ServerKey = "SB-Mid-server-TEST-ONLY"

// Server fakes Midtrans. A Snap transaction does not exist for the status
// API until SetStatus is called (as when the buyer has not chosen a method).
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	tx       map[string]*Tx
	snap     map[string]map[string]any // last Snap request body per order_id
	calls    []string                  // "METHOD /path"
	badAuth  int
	failNext map[string]int // path prefix → number of 500 answers to give
}

// Tx is the fake state of one transaction.
type Tx struct {
	Status      string // pending, settlement, capture, deny, cancel, expire, failure, refund
	Fraud       string
	PaymentType string
	Amount      int64
}

func New(t testing.TB) *Server {
	s := &Server{tx: map[string]*Tx{}, snap: map[string]map[string]any{}, failNext: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)

	if user, pass, ok := r.BasicAuth(); !ok || user != ServerKey || pass != "" {
		s.badAuth++
		writeJSON(w, http.StatusUnauthorized, map[string]any{"status_code": "401", "status_message": "Access denied"})
		return
	}
	for prefix, n := range s.failNext {
		if n > 0 && strings.HasPrefix(r.URL.Path, prefix) {
			s.failNext[prefix] = n - 1
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/snap/v1/transactions":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error_messages": []string{"bad json"}})
			return
		}
		td, _ := body["transaction_details"].(map[string]any)
		id, _ := td["order_id"].(string)
		if _, used := s.snap[id]; used {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error_messages": []string{"order_id has already been taken"}})
			return
		}
		s.snap[id] = body
		writeJSON(w, http.StatusCreated, map[string]any{
			"token": "tok-" + id, "redirect_url": s.URL + "/snap/v4/redirection/tok-" + id,
		})

	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status"):
		s.writeStatus(w, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/status"))

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/cancel")
		tx := s.tx[id]
		switch {
		case tx == nil:
			writeJSON(w, http.StatusNotFound, map[string]any{"status_code": "404", "status_message": "Transaction doesn't exist."})
		case tx.Status == "capture" || tx.Status == "authorize" || (tx.Status == "pending" && tx.PaymentType == "credit_card"):
			tx.Status = "cancel"
			s.writeStatus(w, id)
		default:
			writeJSON(w, http.StatusOK, map[string]any{"status_code": "412", "status_message": "Merchant cannot modify the status of the transaction"})
		}

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/expire"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/expire")
		if tx := s.tx[id]; tx != nil && tx.Status == "pending" {
			tx.Status = "expire"
			s.writeStatus(w, id)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status_code": "412", "status_message": "Merchant cannot modify the status of the transaction"})

	default:
		http.NotFound(w, r)
	}
}

func (s *Server) writeStatus(w http.ResponseWriter, id string) {
	tx := s.tx[id]
	if tx == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"status_code": "404", "status_message": "Transaction doesn't exist."})
		return
	}
	writeJSON(w, http.StatusOK, s.statusBody(id, tx))
}

func (s *Server) statusBody(id string, tx *Tx) map[string]any {
	code := map[string]string{"settlement": "200", "capture": "200", "pending": "201", "expire": "407"}[tx.Status]
	if code == "" {
		code = "202"
	}
	amount := fmt.Sprintf("%d.00", tx.Amount)
	body := map[string]any{
		"status_code":        code,
		"status_message":     "Success, transaction is found",
		"order_id":           id,
		"transaction_id":     "trx-" + id,
		"transaction_status": tx.Status,
		"payment_type":       tx.PaymentType,
		"gross_amount":       amount,
		"transaction_time":   "2026-11-03 10:20:00",
		"signature_key":      Signature(id, code, amount),
	}
	if tx.Fraud != "" {
		body["fraud_status"] = tx.Fraud
	}
	if tx.Status == "settlement" {
		body["settlement_time"] = "2026-11-03 10:21:30"
	}
	return body
}

// SetStatus sets (or creates) the provider-side transaction.
func (s *Server) SetStatus(orderID, status, paymentType string, amount int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tx[orderID] = &Tx{Status: status, PaymentType: paymentType, Amount: amount}
}

// SetFraud sets the fraud status of an existing transaction.
func (s *Server) SetFraud(orderID, fraud string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tx[orderID].Fraud = fraud
}

// Status returns the provider-side status, "" when there is no transaction.
func (s *Server) Status(orderID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tx := s.tx[orderID]; tx != nil {
		return tx.Status
	}
	return ""
}

// Notification is a correctly signed HTTP notification body for the
// transaction's current state.
func (s *Server) Notification(orderID string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.statusBody(orderID, s.tx[orderID]))
	return raw
}

// SnapRequest returns the last Snap request body for orderID.
func (s *Server) SnapRequest(orderID string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap[orderID]
}

// Count returns how many calls matched "METHOD /path-prefix".
func (s *Server) Count(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// BadAuth returns the number of requests with a wrong server key.
func (s *Server) BadAuth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.badAuth
}

// FailNext makes the next n requests whose path starts with prefix answer
// 503 (provider outage).
func (s *Server) FailNext(prefix string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[prefix] = n
}

// Signature is the Midtrans notification signature for ServerKey.
func Signature(orderID, statusCode, grossAmount string) string {
	sum := sha512.Sum512([]byte(orderID + statusCode + grossAmount + ServerKey))
	return hex.EncodeToString(sum[:])
}

// BasicAuth is the Authorization header value Midtrans expects.
func BasicAuth() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(ServerKey+":"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
