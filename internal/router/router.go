// Package router resolves an inbound request to a concrete upstream target:
// which provider handles it and under which model. Resolution is deterministic
// in v1 (no complexity heuristic): it consults the tier header, the request
// model, the per key default alias, and finally the config default alias.
package router

import (
	"fmt"

	"github.com/tylerpearson/llm-gateway/internal/config"
	"github.com/tylerpearson/llm-gateway/internal/provider"
)

// Target is a resolved upstream destination.
type Target struct {
	Provider string
	Model    string
	Shape    provider.Shape
}

// Router maps aliases and concrete models to provider targets.
type Router struct {
	aliases      map[string]config.Route
	defaultAlias string
	shapes       map[string]provider.Shape
}

// New builds a Router from routing config and the wire shape of each configured
// provider (keyed by provider name).
func New(routing config.Routing, shapes map[string]provider.Shape) *Router {
	return &Router{
		aliases:      routing.Aliases,
		defaultAlias: routing.DefaultAlias,
		shapes:       shapes,
	}
}

// Resolve picks a target. tier is the x-llm-tier header (may be empty),
// reqModel is the model named in the request body (may be an alias, a concrete
// model, or empty), and keyDefault is the authenticated key's default alias.
//
// Precedence for choosing the alias: tier header, then request model when it is
// an alias, then the key default, then the config default. When the request
// model is a concrete (non alias) name it passes through to the chosen alias's
// provider under that concrete model.
func (r *Router) Resolve(reqModel, tier, keyDefault string) (Target, error) {
	alias := r.pickAlias(reqModel, tier, keyDefault)
	route, ok := r.aliases[alias]
	if !ok {
		return Target{}, fmt.Errorf("router: cannot resolve a route for model %q (alias %q unknown)", reqModel, alias)
	}

	model := route.Model
	if reqModel != "" && !r.isAlias(reqModel) {
		model = reqModel
	}

	shape, ok := r.shapes[route.Provider]
	if !ok {
		return Target{}, fmt.Errorf("router: provider %q (alias %q) is not configured", route.Provider, alias)
	}
	return Target{Provider: route.Provider, Model: model, Shape: shape}, nil
}

func (r *Router) pickAlias(reqModel, tier, keyDefault string) string {
	if r.isAlias(tier) {
		return tier
	}
	if r.isAlias(reqModel) {
		return reqModel
	}
	if r.isAlias(keyDefault) {
		return keyDefault
	}
	return r.defaultAlias
}

func (r *Router) isAlias(name string) bool {
	if name == "" {
		return false
	}
	_, ok := r.aliases[name]
	return ok
}
