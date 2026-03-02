package graph

import (
	"testing"

	"github.com/cptaffe/9init/internal/config"
)

func svc(name string, after ...string) *config.Service {
	return &config.Service{Name: name, Socket: name, After: after}
}

func TestTopoOrder(t *testing.T) {
	// fontsrv → (no deps)
	// acme    → (no deps, watched)
	// styles  → acme
	// lsp     → acme, styles
	services := []*config.Service{
		svc("lsp", "acme", "styles"),
		svc("styles", "acme"),
		svc("fontsrv"),
		{Name: "acme", Socket: "acme", Watch: true},
	}

	g, err := Build(services)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	order := g.Order()
	pos := map[string]int{}
	for i, s := range order {
		pos[s.Name] = i
	}

	check := func(before, after string) {
		t.Helper()
		if pos[before] >= pos[after] {
			t.Errorf("expected %s before %s in topo order (got positions %d, %d)",
				before, after, pos[before], pos[after])
		}
	}
	check("acme", "styles")
	check("acme", "lsp")
	check("styles", "lsp")
}

func TestDependents(t *testing.T) {
	services := []*config.Service{
		svc("acme"),
		svc("styles", "acme"),
		svc("treesitter", "styles"),
		svc("lsp", "acme", "styles"),
		svc("hotkey", "acme"),
	}
	g, err := Build(services)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	deps := g.Dependents("acme")
	names := map[string]bool{}
	for _, s := range deps {
		names[s.Name] = true
	}

	for _, want := range []string{"styles", "treesitter", "lsp", "hotkey"} {
		if !names[want] {
			t.Errorf("Dependents(acme) missing %q; got %v", want, names)
		}
	}
	if names["acme"] {
		t.Error("Dependents should not include the named service itself")
	}
}

func TestDependentTopoOrder(t *testing.T) {
	// styles → acme; treesitter → styles
	// When acme crashes, we should stop treesitter before styles.
	services := []*config.Service{
		svc("acme"),
		svc("styles", "acme"),
		svc("treesitter", "styles"),
	}
	g, err := Build(services)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	deps := g.Dependents("acme")
	pos := map[string]int{}
	for i, s := range deps {
		pos[s.Name] = i
	}
	// In topo order: styles before treesitter.
	if pos["styles"] >= pos["treesitter"] {
		t.Errorf("expected styles before treesitter in Dependents result")
	}
}

func TestCycleDetected(t *testing.T) {
	services := []*config.Service{
		svc("a", "b"),
		svc("b", "a"),
	}
	_, err := Build(services)
	if err == nil {
		t.Error("expected cycle error, got nil")
	}
}

func TestUnknownDependency(t *testing.T) {
	services := []*config.Service{
		svc("a", "nonexistent"),
	}
	_, err := Build(services)
	if err == nil {
		t.Error("expected unknown-dependency error, got nil")
	}
}

func TestSelfDependency(t *testing.T) {
	services := []*config.Service{
		svc("a", "a"),
	}
	_, err := Build(services)
	if err == nil {
		t.Error("expected self-dependency error, got nil")
	}
}

func TestDuplicateService(t *testing.T) {
	services := []*config.Service{
		svc("a"),
		svc("a"),
	}
	_, err := Build(services)
	if err == nil {
		t.Error("expected duplicate error, got nil")
	}
}
