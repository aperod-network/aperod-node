package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/sale"
	"github.com/aperod/aperod/store"
)

// SetSaleConfig is local operator configuration, never a payment API input.
// Wire before serving requests. Nil leaves every settlement endpoint disabled.
func (s *Server) SetSaleConfig(m *store.SaleMigration, ready func() bool, finalized ...func(uint64, crypto.Hash32) bool) {
	s.saleMigration = m
	s.saleProducerReady = ready
	if len(finalized) == 1 {
		s.saleFinalized = finalized[0]
	}
}

func (s *Server) isSaleFinalized(height uint64, hash crypto.Hash32) bool {
	f := s.saleFinalized
	if f == nil {
		f = s.lpodFinalized
	}
	return f != nil && f(height, hash)
}

func saleHash(text string) (crypto.Hash32, error) {
	var result crypto.Hash32
	if len(text) != 64 {
		return result, fmt.Errorf("invalid hash")
	}
	raw, err := hex.DecodeString(text)
	if err != nil {
		return result, err
	}
	copy(result[:], raw)
	if result == (crypto.Hash32{}) {
		return result, fmt.Errorf("zero hash")
	}
	return result, nil
}

func saleHex(hash crypto.Hash32) string { return hex.EncodeToString(hash[:]) }

func decodeSaleActivation(raw []byte) (*core.Block, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("activation body unavailable")
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, err
	}
	return &block, nil
}

func (s *Server) activeSale(requireProducer bool) (*sale.State, error) {
	m := s.saleMigration
	if m == nil || s.blockStore == nil || s.myKey == nil || (s.saleFinalized == nil && s.lpodFinalized == nil) ||
		(requireProducer && (s.saleProducerReady == nil || !s.saleProducerReady())) ||
		!s.myKey.Public().Equals(m.Checkpoint.SaleAuthority) {
		return nil, fmt.Errorf("authorized sale authority unavailable")
	}
	cp := m.Checkpoint
	if _, err := sale.Begin(cp, cp.Genesis, cp.Parent, cp.Height, m.Trusted); err != nil {
		return nil, err
	}
	state, err := s.blockStore.LoadSaleState()
	if err != nil {
		return nil, err
	}
	if state == nil || state.Checkpoint != cp.Message() {
		return nil, fmt.Errorf("sale checkpoint inactive")
	}
	hash, height, err := s.blockStore.GetTip()
	if err != nil {
		return nil, err
	}
	if state.Hash != hash || state.Height != height {
		return nil, fmt.Errorf("sale state is not canonical tip")
	}
	raw, err := s.blockStore.GetRawBlockByHeight(cp.Height)
	if err != nil {
		return nil, err
	}
	activation, err := decodeSaleActivation(raw)
	if err != nil || activation.Header.PrevHash != cp.Parent || !s.isSaleFinalized(cp.Height, activation.Hash()) {
		return nil, fmt.Errorf("sale activation not finalized")
	}
	return state, nil
}

func saleJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func saleBody(r *http.Request, value any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16385))
	if err != nil || len(raw) > 16384 {
		return fmt.Errorf("invalid request size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return fmt.Errorf("trailing request data")
	}
	return nil
}

func (s *Server) saleReady(w http.ResponseWriter, r *http.Request, method string, requireProducer bool) bool {
	if r.Method != method {
		saleJSON(w, 405, map[string]string{"error": "method not allowed"})
		return false
	}
	if _, err := s.activeSale(requireProducer); err != nil {
		saleJSON(w, 503, map[string]string{"error": "AUTHORIZED_APRO_SETTLEMENT_UNAVAILABLE"})
		return false
	}
	return true
}

func (s *Server) restSaleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.saleReady(w, r, http.MethodGet, false) {
		return
	}
	state, err := s.activeSale(false)
	if err != nil {
		saleJSON(w, 503, map[string]string{"error": "sale state changed"})
		return
	}
	reserved, err := s.blockStore.ReservedSaleOrders()
	if err != nil || reserved > state.Remaining {
		saleJSON(w, 503, map[string]string{"error": "sale obligations inconsistent"})
		return
	}
	cp := s.saleMigration.Checkpoint
	saleJSON(w, 200, map[string]any{"ready": s.saleProducerReady != nil && s.saleProducerReady(), "checkpoint": saleHex(cp.Message()),
		"genesis": saleHex(cp.Genesis), "activation_height": cp.Height, "tip_hash": saleHex(state.Hash),
		"tip_height": state.Height, "remaining_napro": strconv.FormatUint(state.Remaining, 10),
		"reserved_napro": strconv.FormatUint(reserved, 10), "available_napro": strconv.FormatUint(state.Remaining-reserved, 10),
		"allocation_basis": saleAllocationBasis(s.saleMigration.Reconciliation), "historical_supply_verified": false,
		"sold_napro": strconv.FormatUint(state.Sold, 10)})
}

// Private, authenticated diagnostics: closed codes only, never keys, endpoint
// addresses, checkpoint hashes, wallet data or a claim of settled payment.
func (s *Server) restSaleReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		saleJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	code := "READY"
	switch {
	case s.saleMigration == nil:
		code = "SALE_NOT_ACTIVATED"
	case s.blockStore == nil:
		code = "NODE_UNAVAILABLE"
	case s.myKey == nil || !s.myKey.Public().Equals(s.saleMigration.Checkpoint.SaleAuthority):
		code = "NO_READY_PRODUCER"
	default:
		if _, err := s.activeSale(false); err != nil {
			code = "CHECKPOINT_LEDGER_MISMATCH"
			if err.Error() == "sale activation not finalized" || err.Error() == "authorized sale authority unavailable" {
				code = "NO_READY_PRODUCER"
			}
		} else if s.saleProducerReady == nil || !s.saleProducerReady() {
			code = "NO_READY_PRODUCER"
		}
	}
	saleJSON(w, 200, map[string]any{"code": code, "ready": code == "READY"})
}

func saleAllocationBasis(r sale.Reconciliation) string {
	if r.AllocationEpoch != nil {
		return "operator_attested_lpod_epoch"
	}
	if r.NetAllocation != nil {
		return "authority_attested_net_holdings"
	}
	return "legacy_nominal_declaration"
}

func (s *Server) restSaleReserve(w http.ResponseWriter, r *http.Request) {
	if !s.saleReady(w, r, http.MethodPost, true) {
		return
	}
	var body struct {
		Order      string `json:"order"`
		Checkpoint string `json:"checkpoint"`
		Address    string `json:"address"`
		Amount     string `json:"amount_napro"`
		QuoteUSD   string `json:"quote_usd"`
	}
	if saleBody(r, &body) != nil {
		saleJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	order, err := saleHash(body.Order)
	cp, err2 := saleHash(body.Checkpoint)
	amount, err3 := strconv.ParseUint(body.Amount, 10, 64)
	if err != nil || err2 != nil || err3 != nil || amount == 0 || strconv.FormatUint(amount, 10) != body.Amount {
		saleJSON(w, 400, map[string]string{"error": "invalid exact intent"})
		return
	}
	err = s.blockStore.ReserveSaleOrder(store.SaleOrder{Order: order, Checkpoint: cp, Recipient: crypto.Address(body.Address), Amount: amount, QuoteUSD: body.QuoteUSD}, s.saleMigration.Checkpoint.Network)
	if err != nil {
		saleJSON(w, 409, map[string]string{"error": "reservation refused"})
		return
	}
	saleJSON(w, 200, map[string]string{"order": body.Order, "status": "reserved"})
}

func (s *Server) restSalePaid(w http.ResponseWriter, r *http.Request) {
	if !s.saleReady(w, r, http.MethodPost, true) {
		return
	}
	var body struct {
		Order   string `json:"order"`
		Payment string `json:"payment"`
	}
	if saleBody(r, &body) != nil {
		saleJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	order, err := saleHash(body.Order)
	payment, err2 := saleHash(body.Payment)
	if err != nil || err2 != nil {
		saleJSON(w, 400, map[string]string{"error": "invalid payment identity"})
		return
	}
	reserved, readErr := s.blockStore.SaleOrder(order)
	if readErr != nil || reserved == nil || reserved.Invoice == nil || !reserved.Invoice.Valid() ||
		store.SaleProviderPayment(reserved.Invoice.ProviderID) != payment {
		saleJSON(w, 409, map[string]string{"error": "payment does not match immutable invoice"})
		return
	}
	if err := s.blockStore.MarkSaleOrderPaid(order, payment); err != nil {
		saleJSON(w, 409, map[string]string{"error": "payment obligation refused"})
		return
	}
	saleJSON(w, 200, map[string]string{"order": body.Order, "status": "payment_confirmed"})
}

func (s *Server) restSaleReceipt(w http.ResponseWriter, r *http.Request) {
	if !s.saleReady(w, r, http.MethodGet, false) {
		return
	}
	id, err := saleHash(r.URL.Query().Get("order"))
	if err != nil {
		saleJSON(w, 400, map[string]string{"error": "invalid order"})
		return
	}
	order, err := s.blockStore.SaleOrder(id)
	if err != nil {
		saleJSON(w, 503, map[string]string{"error": "order storage unavailable"})
		return
	}
	if order == nil {
		saleJSON(w, 404, map[string]string{"error": "order not found"})
		return
	}
	delivery, err := s.blockStore.SaleOrderDelivery(id)
	if err != nil {
		saleJSON(w, 503, map[string]string{"error": "canonical delivery unavailable"})
		return
	}
	result := map[string]any{"order": saleHex(id), "payment": saleHex(order.Payment),
		"checkpoint": saleHex(order.Checkpoint), "amount_napro": strconv.FormatUint(order.Amount, 10), "status": "reserved"}
	if order.Invoice != nil {
		result["invoice"] = order.Invoice
	}
	if order.QuoteUSD != "" {
		result["quote_usd"] = order.QuoteUSD
	}
	if order.Payment != (crypto.Hash32{}) {
		result["status"] = "payment_confirmed"
	}
	if delivery != nil {
		result["status"] = "included"
		result["tx_hash"] = saleHex(delivery.Transaction)
		result["block_hash"] = saleHex(delivery.Block)
		result["block_height"] = delivery.Height
		if s.isSaleFinalized(delivery.Height, delivery.Block) {
			result["status"] = "finalized"
			result["owner_challenge"] = store.SaleRecoveryChallenge(s.saleMigration.Checkpoint.Genesis, order, delivery)
			recovered, recoveryErr := s.blockStore.SaleOrderRecovery(id)
			if recoveryErr != nil {
				saleJSON(w, 503, map[string]string{"error": "owner recovery unavailable"})
				return
			}
			if recovered != nil && recovered.Block == delivery.Block && recovered.Transaction == delivery.Transaction {
				result["status"] = "completed"
				delete(result, "owner_challenge")
			}
		}
	}
	// Finality alone is not completion; an owner-signed recovery is required.
	saleJSON(w, 200, result)
}

func (s *Server) restSaleRecover(w http.ResponseWriter, r *http.Request) {
	if !s.saleReady(w, r, http.MethodPost, false) {
		return
	}
	var body struct {
		Order       string `json:"order"`
		Block       string `json:"block_hash"`
		Transaction string `json:"tx_hash"`
		Signature   string `json:"signature"`
	}
	if saleBody(r, &body) != nil {
		saleJSON(w, 400, map[string]string{"error": "invalid owner receipt"})
		return
	}
	id, err1 := saleHash(body.Order)
	block, err2 := saleHash(body.Block)
	tx, err3 := saleHash(body.Transaction)
	if err1 != nil || err2 != nil || err3 != nil {
		saleJSON(w, 400, map[string]string{"error": "invalid canonical hashes"})
		return
	}
	delivery, err := s.blockStore.SaleOrderDelivery(id)
	if err != nil || delivery == nil || delivery.Block != block || delivery.Transaction != tx ||
		!s.isSaleFinalized(delivery.Height, delivery.Block) {
		saleJSON(w, 409, map[string]string{"error": "canonical finalized delivery required"})
		return
	}
	if err := s.blockStore.RecordSaleRecovery(s.saleMigration.Checkpoint.Genesis, id, block, tx, body.Signature); err != nil {
		saleJSON(w, 409, map[string]string{"error": "owner recovery proof rejected"})
		return
	}
	saleJSON(w, 200, map[string]string{"order": body.Order, "status": "owner_recovered"})
}

func (s *Server) restSaleInvoice(w http.ResponseWriter, r *http.Request) {
	if !s.saleReady(w, r, http.MethodPost, true) {
		return
	}
	var body struct {
		Order   string            `json:"order"`
		Invoice store.SaleInvoice `json:"invoice"`
	}
	if saleBody(r, &body) != nil {
		saleJSON(w, 400, map[string]string{"error": "invalid invoice"})
		return
	}
	order, err := saleHash(body.Order)
	if err != nil {
		saleJSON(w, 400, map[string]string{"error": "invalid order"})
		return
	}
	if err := s.blockStore.BindSaleInvoice(order, body.Invoice); err != nil {
		saleJSON(w, 409, map[string]string{"error": "invoice binding refused"})
		return
	}
	saleJSON(w, 200, map[string]string{"order": body.Order, "status": "invoice_bound"})
}
