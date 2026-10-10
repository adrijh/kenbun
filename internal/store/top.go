package store

import "sort"

// FuncStat is one row of the top-functions table.
type FuncStat struct {
	Name  string
	Self  int64 // weight where the function is the leaf
	Total int64 // weight where the function is anywhere in the stack, once per stack
}

// Top computes per-function self/total weight from aggregated stacks, sorted by
// self then total. limit <= 0 returns everything.
func Top(stacks []StackWeight, limit int) []FuncStat {
	self := map[string]int64{}
	total := map[string]int64{}
	for _, st := range stacks {
		if len(st.Frames) == 0 {
			continue
		}
		self[st.Frames[0]] += st.Weight

		seen := make(map[string]struct{}, len(st.Frames))
		for _, f := range st.Frames { // recursion: count each function once per stack
			if _, ok := seen[f]; ok {
				continue
			}
			seen[f] = struct{}{}
			total[f] += st.Weight
		}
	}

	out := make([]FuncStat, 0, len(total))
	for name, t := range total {
		out = append(out, FuncStat{Name: name, Self: self[name], Total: t})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Self != out[j].Self {
			return out[i].Self > out[j].Self
		}
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
