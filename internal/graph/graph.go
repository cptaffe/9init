// Package graph builds and queries the service dependency graph.
package graph

import (
	"fmt"
	"sort"

	"github.com/cptaffe/9init/internal/config"
)

// Graph holds the validated, topologically sorted service dependency graph.
type Graph struct {
	services map[string]*config.Service
	// rdeps[name] = names of services that directly depend on name.
	rdeps map[string][]string
	order []*config.Service // topological order, deps before dependents
}

// Build validates all dependency references and returns a Graph.
// Returns an error if any after reference is unknown or a cycle exists.
func Build(services []*config.Service) (*Graph, error) {
	g := &Graph{
		services: make(map[string]*config.Service, len(services)),
		rdeps:    make(map[string][]string, len(services)),
	}
	for _, s := range services {
		if _, dup := g.services[s.Name]; dup {
			return nil, fmt.Errorf("duplicate service name %q", s.Name)
		}
		g.services[s.Name] = s
	}

	// Validate after references and build reverse-dependency map.
	for _, s := range services {
		seen := map[string]bool{}
		for _, dep := range s.After {
			if _, ok := g.services[dep]; !ok {
				return nil, fmt.Errorf("service %q: unknown dependency %q", s.Name, dep)
			}
			if dep == s.Name {
				return nil, fmt.Errorf("service %q: depends on itself", s.Name)
			}
			if seen[dep] {
				return nil, fmt.Errorf("service %q: duplicate dependency %q", s.Name, dep)
			}
			seen[dep] = true
			g.rdeps[dep] = append(g.rdeps[dep], s.Name)
		}
	}

	var err error
	g.order, err = topoSort(services, g.services, g.rdeps)
	if err != nil {
		return nil, err
	}
	return g, nil
}

// Order returns all services in topological order: dependencies appear before
// the services that depend on them.
func (g *Graph) Order() []*config.Service {
	out := make([]*config.Service, len(g.order))
	copy(out, g.order)
	return out
}

// Service returns the named service, or nil if it does not exist.
func (g *Graph) Service(name string) *config.Service {
	return g.services[name]
}

// Dependents returns all services that transitively depend on name, in
// topological order. The named service itself is not included.
// Returns nil if name is not in the graph.
func (g *Graph) Dependents(name string) []*config.Service {
	if _, ok := g.services[name]; !ok {
		return nil
	}
	// BFS over the reverse-dependency edges.
	visited := map[string]bool{name: true}
	queue := []string{name}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dep := range g.rdeps[cur] {
			if !visited[dep] {
				visited[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	// Return in topological order so callers can stop/start in the right sequence.
	var out []*config.Service
	for _, s := range g.order {
		if visited[s.Name] && s.Name != name {
			out = append(out, s)
		}
	}
	return out
}

// topoSort implements Kahn's algorithm. It returns the services in dependency
// order and an error if a cycle is detected.
func topoSort(
	services []*config.Service,
	byName map[string]*config.Service,
	rdeps map[string][]string,
) ([]*config.Service, error) {
	// inDegree[name] = number of unsatisfied dependencies.
	inDegree := make(map[string]int, len(services))
	for _, s := range services {
		inDegree[s.Name] = len(s.After)
	}

	// Seed the queue with all zero-in-degree services, sorted for determinism.
	var queue []string
	for _, s := range services {
		if inDegree[s.Name] == 0 {
			queue = append(queue, s.Name)
		}
	}
	sort.Strings(queue)

	var order []*config.Service
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		order = append(order, byName[name])

		// Reduce in-degree for each service that depends on name.
		newly := []string{}
		for _, dep := range rdeps[name] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				newly = append(newly, dep)
			}
		}
		sort.Strings(newly) // determinism
		queue = append(queue, newly...)
	}

	if len(order) != len(services) {
		return nil, fmt.Errorf("dependency cycle detected among services")
	}
	return order, nil
}
