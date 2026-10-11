package store

import "sort"

// Node is one frame in a call tree (flamegraph): root first, unlike Frames.
type Node struct {
	Name     string
	Self     int64
	Total    int64
	Lines    map[string]int64 // Total by the file:line this frame was on; nil if unknown
	Children []*Node          // heaviest first

	byName map[string]*Node // build-time child lookup
}

// Tree merges stacks into a call tree under a synthetic "all" root.
// Unlike Top, recursion is not deduplicated: each level is its own node.
func Tree(stacks []StackWeight) *Node {
	root := &Node{Name: "all"}
	for _, st := range stacks {
		root.Total += st.Weight
		n := root
		for i := len(st.Frames) - 1; i >= 0; i-- {
			n = n.child(st.Frames[i])
			n.Total += st.Weight
			if l := loc(st.Locs, i); l != "" {
				if n.Lines == nil {
					n.Lines = map[string]int64{}
				}
				n.Lines[l] += st.Weight
			}
		}
		n.Self += st.Weight
	}
	root.sort()
	return root
}

func (n *Node) child(name string) *Node {
	if c, ok := n.byName[name]; ok {
		return c
	}
	if n.byName == nil {
		n.byName = map[string]*Node{}
	}
	c := &Node{Name: name}
	n.byName[name] = c
	n.Children = append(n.Children, c)
	return c
}

func (n *Node) sort() {
	n.byName = nil
	sort.Slice(n.Children, func(i, j int) bool {
		a, b := n.Children[i], n.Children[j]
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.Name < b.Name
	})
	for _, c := range n.Children {
		c.sort()
	}
}
