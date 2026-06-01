// Package tenant is the multi-tenancy boundary for the game-provider platform.
//
// The platform is a GAME PROVIDER (an RGS), not the casino. Its tenants are
// OPERATORS — the licensed casino brands that own the player relationship and
// the wallet. This module holds per-operator config and the registry that
// resolves an operatorId to its config + wallet client, or rejects an unknown
// operator outright (the isolation boundary fails closed).
//
// Everything operator-specific (enabled games, allowed assets, bet limits,
// callback secret, wallet endpoint dialect) lives here at the edge; the engine
// core stays operator-agnostic.
package tenant

import (
	"fmt"
	"math/big"

	"roundrunner/internal/asset"
	"roundrunner/internal/seamless"
)

// UnknownOperatorError is returned when resolving an operatorId that was never
// registered. Resolving fails closed.
type UnknownOperatorError struct{ OperatorID string }

func (e *UnknownOperatorError) Error() string {
	return fmt.Sprintf("unknown operator: %s", e.OperatorID)
}

// ConfigError reports an invalid tenant configuration.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return "tenant: " + e.Msg }

// Limit is a per-asset stake override (base units).
type Limit struct {
	Min *big.Int
	Max *big.Int
}

// Config is per-operator configuration. It is treated as immutable: changing a
// tenant means registering a new config, not mutating a live one.
type Config struct {
	operatorID     string
	displayName    string
	enabledGames   map[string]struct{}
	allowedAssets  map[string]struct{}
	limits         map[string]Limit // keyed by asset key
	callbackSecret string
	jurisdiction   string
}

// Spec is the constructor input for a tenant Config.
type Spec struct {
	OperatorID     string
	DisplayName    string
	EnabledGames   []string
	AllowedAssets  []string // asset keys
	Limits         map[string]Limit
	CallbackSecret string
	Jurisdiction   string
}

// NewConfig validates and builds a tenant Config.
func NewConfig(s Spec) (Config, error) {
	if s.OperatorID == "" {
		return Config{}, &ConfigError{"operatorID required"}
	}
	if len(s.EnabledGames) == 0 {
		return Config{}, &ConfigError{"enabledGames must be non-empty"}
	}
	if len(s.AllowedAssets) == 0 {
		return Config{}, &ConfigError{"allowedAssets must be non-empty"}
	}
	// A per-operator secret is non-negotiable: one operator's key must never
	// validate another's seamless traffic.
	if s.CallbackSecret == "" {
		return Config{}, &ConfigError{"callbackSecret required"}
	}
	games := map[string]struct{}{}
	for _, g := range s.EnabledGames {
		games[g] = struct{}{}
	}
	assets := map[string]struct{}{}
	for _, a := range s.AllowedAssets {
		assets[a] = struct{}{}
	}
	jur := s.Jurisdiction
	if jur == "" {
		jur = "unspecified"
	}
	name := s.DisplayName
	if name == "" {
		name = s.OperatorID
	}
	return Config{
		operatorID:     s.OperatorID,
		displayName:    name,
		enabledGames:   games,
		allowedAssets:  assets,
		limits:         s.Limits,
		callbackSecret: s.CallbackSecret,
		jurisdiction:   jur,
	}, nil
}

func (c Config) OperatorID() string     { return c.operatorID }
func (c Config) DisplayName() string    { return c.displayName }
func (c Config) Jurisdiction() string   { return c.jurisdiction }
func (c Config) CallbackSecret() string { return c.callbackSecret }

func (c Config) AllowsGame(gameName string) bool {
	_, ok := c.enabledGames[gameName]
	return ok
}

func (c Config) AllowsAsset(a asset.Asset) bool {
	_, ok := c.allowedAssets[a.Key()]
	return ok
}

// LimitFor returns this tenant's per-asset stake override, if any. The second
// return is false when the game's own min/max applies.
func (c Config) LimitFor(a asset.Asset) (Limit, bool) {
	l, ok := c.limits[a.Key()]
	return l, ok
}

// Tenant pairs a resolved config with that operator's seamless wallet client.
type Tenant struct {
	Config Config
	Wallet seamless.OperatorWallet
}

// Registry is the one abstraction the engine depends on for tenancy. It maps an
// operatorId to that operator's config + wallet client, or rejects an unknown
// operator. The wallet carries that operator's endpoint + credentials.
type Registry struct {
	tenants map[string]Tenant
}

func NewRegistry() *Registry { return &Registry{tenants: map[string]Tenant{}} }

// Register adds an operator. It is an error to register the same operator twice
// or to pass a nil wallet.
func (r *Registry) Register(c Config, w seamless.OperatorWallet) error {
	if w == nil {
		return &ConfigError{"wallet must implement the seamless interface"}
	}
	if _, ok := r.tenants[c.operatorID]; ok {
		return &ConfigError{"operator already registered: " + c.operatorID}
	}
	r.tenants[c.operatorID] = Tenant{Config: c, Wallet: w}
	return nil
}

func (r *Registry) Has(operatorID string) bool {
	_, ok := r.tenants[operatorID]
	return ok
}

// Resolve returns a tenant or an *UnknownOperatorError.
func (r *Registry) Resolve(operatorID string) (Tenant, error) {
	t, ok := r.tenants[operatorID]
	if !ok {
		return Tenant{}, &UnknownOperatorError{OperatorID: operatorID}
	}
	return t, nil
}

func (r *Registry) OperatorIDs() []string {
	out := make([]string, 0, len(r.tenants))
	for id := range r.tenants {
		out = append(out, id)
	}
	return out
}
