package mcp

import (
	"context"
	"errors"
	"fmt"

	"corpos/internal/tool"
)

// Server is one mounted MCP server behind the aggregator: a tool.Provider that
// dispatches the server's surfaces, plus the tool specs it offers (already
// enriched from the live substrate for a toolkit server, or static for the web
// server). The Name is for diagnostics only — routing is by surface name, which
// is what the model addresses.
type Server struct {
	// Name is the server's mount name (diagnostics; collisions are reported by it).
	Name string
	// Provider dispatches tool calls for this server's surfaces.
	Provider tool.Provider
	// Specs are the tool specs this server offers (one per surface it owns).
	Specs []tool.Spec
}

// Aggregator mounts N MCP servers behind one tool.Provider and routes each tool
// call to the server that owns the call's surface. It is the multi-server seam
// the design names (§1.2: "aggregating tool.Provider over []Provider"; §5#4): the
// loop dispatches through one provider while corpos draws surfaces from the
// toolkit, the web surface, and future servers. The offered spec set is the UNION
// of the mounted servers' specs, which per-profile projection (mcp.Project) then
// narrows to each worker's declared subset.
type Aggregator struct {
	routes map[string]tool.Provider // surface name -> owning provider
	owner  map[string]string        // surface name -> owning server name (diagnostics)
	specs  []tool.Spec              // the merged union, in mount order
}

// ensure *Aggregator satisfies the provider seam at compile time.
var _ tool.Provider = (*Aggregator)(nil)

// NewAggregator builds the routing table from the mounted servers. Surfaces must
// be globally unique across servers: a surface offered by two servers is a config
// error (the route would be ambiguous), reported with both server names. Mounting
// zero servers, or a server with no provider, is rejected. The merged spec set
// preserves the servers' argument order, and each server's specs in their given
// order, so the offered catalog is deterministic.
func NewAggregator(servers ...Server) (*Aggregator, error) {
	if len(servers) == 0 {
		return nil, errors.New("aggregator needs at least one server")
	}
	a := &Aggregator{
		routes: make(map[string]tool.Provider),
		owner:  make(map[string]string),
	}
	for _, s := range servers {
		if s.Provider == nil {
			return nil, fmt.Errorf("server %q has no provider", s.Name)
		}
		for _, spec := range s.Specs {
			if prev, dup := a.owner[spec.Name]; dup {
				return nil, fmt.Errorf("surface %q is offered by both server %q and server %q", spec.Name, prev, s.Name)
			}
			a.routes[spec.Name] = s.Provider
			a.owner[spec.Name] = s.Name
			a.specs = append(a.specs, spec)
		}
	}
	return a, nil
}

// Dispatch routes one tool call to the server that owns its surface. A call for an
// unmounted surface fails as a tool error (never out of band) so the loop folds it
// back into the transcript like any other failed dispatch — the same contract the
// underlying providers honor.
func (a *Aggregator) Dispatch(ctx context.Context, call tool.Call) tool.Result {
	p, ok := a.routes[call.Surface]
	if !ok {
		return tool.Fail(call, tool.ClassTool, fmt.Sprintf("no mounted server offers surface %q", call.Surface), 0)
	}
	return p.Dispatch(ctx, call)
}

// Specs returns the union of the mounted servers' tool specs, in mount order.
// This is the full (unprojected) catalog; the caller projects it per profile.
func (a *Aggregator) Specs() []tool.Spec {
	return a.specs
}

// Owner returns the name of the server that owns a surface, and whether the
// surface is mounted at all — a small accessor for diagnostics and tests.
func (a *Aggregator) Owner(surface string) (string, bool) {
	name, ok := a.owner[surface]
	return name, ok
}
