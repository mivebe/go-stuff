// Package api exposes the engine over REST. The HTTP layer is deliberately
// thin: decode a request, call the engine, encode the result. All domain logic
// lives below it. A WebSocket or gRPC surface would wrap the SAME engine the
// same way — the transport is a detail, the engine is the product.
//
// The surface is tenant-aware: every request names an operator, exactly as a
// real game-provider integration would (the operator is authenticated from a
// signed header in production; here it is a body field for the demo).
package api

import (
	"encoding/json"
	"errors"
	"math/big"
	"net/http"

	"roundrunner/internal/asset"
	"roundrunner/internal/game"
	"roundrunner/internal/ledger"
	"roundrunner/internal/rng"
	"roundrunner/internal/round"
	"roundrunner/internal/seamless"
	"roundrunner/internal/tenant"
)

// Server adapts HTTP requests to engine calls.
type Server struct {
	engine *round.Engine
	ledger *ledger.Ledger
}

// NewServer constructs the REST server.
func NewServer(e *round.Engine, l *ledger.Ledger) *Server {
	return &Server{engine: e, ledger: l}
}

// Routes returns the configured mux (Go 1.22 method-aware patterns).
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/rounds", s.handlePlay)
	mux.HandleFunc("GET /v1/ledger", s.handleLedger)
	return mux
}

// playRequest is the JSON body for POST /v1/rounds. Stake is a base-units
// decimal STRING (crypto amounts overflow JSON numbers), tagged with an asset
// key like "USDT:tron".
type playRequest struct {
	OperatorID     string `json:"operator_id"`
	Player         string `json:"player"`
	GameName       string `json:"game"`
	ClientSeed     string `json:"client_seed"`
	IdempotencyKey string `json:"idempotency_key"`
	AssetKey       string `json:"asset"`
	Stake          string `json:"stake"`  // base units, decimal string
	Target         uint32 `json:"target"` // win if roll >= target
}

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	var req playRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a, err := asset.Lookup(req.AssetKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stakeVal, ok := new(big.Int).SetString(req.Stake, 10)
	if !ok {
		http.Error(w, "invalid stake (want a base-units decimal string)", http.StatusBadRequest)
		return
	}
	res, err := s.engine.Execute(r.Context(), round.Request{
		OperatorID:     req.OperatorID,
		PlayerID:       req.Player,
		GameName:       req.GameName,
		ClientSeed:     req.ClientSeed,
		IdempotencyKey: req.IdempotencyKey,
		Bet: game.Bet{
			Stake:  asset.New(a, stakeVal),
			Target: rng.Roll(req.Target),
		},
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	if err := s.ledger.Verify(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s.ledger.Records())
}

// writeError maps domain errors to HTTP status codes.
func writeError(w http.ResponseWriter, err error) {
	var unknownOp *tenant.UnknownOperatorError
	var gameOff *round.GameNotEnabledError
	var assetOff *round.AssetNotAllowedError
	var declined *seamless.DeclinedError
	switch {
	case errors.As(err, &unknownOp) || errors.As(err, &gameOff) || errors.As(err, &assetOff):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.As(err, &declined):
		http.Error(w, err.Error(), http.StatusPaymentRequired)
	default:
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
