package store

import (
	"reflect"
	"testing"
)

func TestTop(t *testing.T) {
	stacks := []StackWeight{
		{Frames: []string{"fib", "fib", "main"}, Weight: 3}, // recursion counts once
		{Frames: []string{"json", "main"}, Weight: 2},
		{Frames: nil, Weight: 5}, // empty stacks are ignored
	}
	want := []FuncStat{
		{Name: "fib", Self: 3, Total: 3},
		{Name: "json", Self: 2, Total: 2},
		{Name: "main", Self: 0, Total: 5},
	}
	if got := Top(stacks, 0); !reflect.DeepEqual(got, want) {
		t.Errorf("Top = %+v, want %+v", got, want)
	}
	if got := Top(stacks, 1); len(got) != 1 || got[0].Name != "fib" {
		t.Errorf("Top(limit=1) = %+v", got)
	}
}
