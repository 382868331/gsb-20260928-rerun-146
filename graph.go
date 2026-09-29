package assetpack

import "sort"

// DependencyGraph returns resource ID -> dependency IDs, including cycles.
func (p *Package) DependencyGraph() map[string][]string {
	out := make(map[string][]string, len(p.manifest.Resources))
	for _, r := range p.manifest.Resources {
		out[r.ID] = append([]string(nil), r.Deps...)
	}
	return out
}

// NestedGraph returns resource ID -> child name/resource ID pairs.
func (p *Package) NestedGraph() map[string][]ChildRef {
	out := make(map[string][]ChildRef, len(p.manifest.Resources))
	for _, r := range p.manifest.Resources {
		out[r.ID] = append([]ChildRef(nil), r.Children...)
	}
	return out
}

// ResourcesInDependencyOrder returns a stable order suitable for loading.
// Cycles are retained and deterministically broken by resource ID.
func (p *Package) ResourcesInDependencyOrder() []string {
	ids := make([]string, 0, len(p.manifest.Resources))
	for _, r := range p.manifest.Resources {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)

	state := make(map[string]int)
	var order []string
	var visit func(string)
	visit = func(id string) {
		if state[id] != 0 {
			return
		}
		state[id] = 1
		r := p.resourceByID(id)
		deps := append([]string(nil), r.Deps...)
		sort.Strings(deps)
		for _, dep := range deps {
			visit(dep)
		}
		state[id] = 2
		order = append(order, id)
	}
	for _, id := range ids {
		visit(id)
	}
	return order
}

func (p *Package) resourceByID(id string) Resource {
	if r, ok := p.findResource(id); ok == nil {
		return r
	}
	return Resource{ID: id}
}
