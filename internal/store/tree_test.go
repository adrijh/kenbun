package store

import "testing"

func TestTree(t *testing.T) {
	root := Tree([]StackWeight{
		{Frames: []string{"fib", "fib", "main"}, Weight: 3},
		{Frames: []string{"json", "main"}, Weight: 2},
		{Frames: []string{"main"}, Weight: 1},
	})
	if root.Total != 6 || len(root.Children) != 1 {
		t.Fatalf("root = %+v", root)
	}
	main := root.Children[0]
	if main.Name != "main" || main.Total != 6 || main.Self != 1 {
		t.Fatalf("main = %+v", main)
	}
	if fib := main.Children[0]; fib.Name != "fib" || fib.Total != 3 || fib.Children[0].Self != 3 {
		t.Fatalf("fib = %+v", fib) // heaviest first, recursion kept as depth
	}
	if js := main.Children[1]; js.Name != "json" || js.Self != 2 {
		t.Fatalf("json = %+v", js)
	}
}
