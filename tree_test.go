package orderbook

import (
	"math/rand/v2"
	"testing"
)

func TestTreeInsertBalancingCases(t *testing.T) {
	cases := []struct {
		name   string
		prices []Price
		root   Price
	}{
		{name: "LL", prices: []Price{30, 20, 10}, root: 20},
		{name: "RR", prices: []Price{10, 20, 30}, root: 20},
		{name: "LR", prices: []Price{30, 10, 20}, root: 20},
		{name: "RL", prices: []Price{10, 30, 20}, root: 20},
		{name: "red uncle recolor", prices: []Price{10, 5, 15, 1}, root: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			side := newTestSide(Buy)
			for _, price := range tc.prices {
				insertTestLevel(t, side, price)
				assertTreeInvariants(t, side)
			}
			if side.root.price != tc.root {
				t.Fatalf("root price=%d, want %d", side.root.price, tc.root)
			}
		})
	}
}

func TestTreeDeleteShapes(t *testing.T) {
	cases := []struct {
		name   string
		prices []Price
		remove Price
	}{
		{name: "leaf", prices: []Price{10, 5, 15}, remove: 5},
		{name: "one child", prices: []Price{10, 5, 15, 12}, remove: 15},
		{name: "two children", prices: []Price{10, 5, 15, 12, 17}, remove: 10},
		{name: "black sibling transitions", prices: []Price{41, 38, 31, 12, 19, 8}, remove: 8},
		{name: "deep successor", prices: []Price{20, 10, 30, 5, 15, 25, 35, 23, 27}, remove: 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			side := newTestSide(Buy)
			for _, price := range tc.prices {
				insertTestLevel(t, side, price)
			}
			level := side.levels[tc.remove]
			if level == nil || !side.removeLevel(level) {
				t.Fatalf("remove %d failed", tc.remove)
			}
			assertTreeInvariants(t, side)
			if side.levels[tc.remove] != nil {
				t.Fatalf("removed price %d remains indexed", tc.remove)
			}
		})
	}
}

func TestTreeBoundaryAndDuplicateOperations(t *testing.T) {
	side := newTestSide(Sell)
	if minimum(side.root) != nil || maximum(side.root) != nil {
		t.Fatal("empty tree has a min or max")
	}
	if side.removeLevel(nil) {
		t.Fatal("removing nil from an empty tree succeeded")
	}
	if side.removeLevel(&priceLevel{price: 10}) {
		t.Fatal("removing a stale level from an empty tree succeeded")
	}

	first := &priceLevel{price: 10}
	if !side.insertLevel(first) {
		t.Fatal("first insert failed")
	}
	if side.insertLevel(&priceLevel{price: 10}) {
		t.Fatal("duplicate price was inserted")
	}
	assertTreeInvariants(t, side)
	if len(side.levels) != 1 || side.levels[10] != first {
		t.Fatalf("duplicate changed index: %+v", side.levels)
	}
	if side.removeLevel(&priceLevel{price: 10}) {
		t.Fatal("removing a distinct node with the same price succeeded")
	}
	if !side.removeLevel(first) {
		t.Fatal("single-node remove failed")
	}
	assertTreeInvariants(t, side)
	if side.removeLevel(first) {
		t.Fatal("repeated remove succeeded")
	}
}

func TestTreeMonotonicInsertAndDelete(t *testing.T) {
	for _, descending := range []bool{false, true} {
		t.Run(map[bool]string{false: "ascending", true: "descending"}[descending], func(t *testing.T) {
			side := newTestSide(Buy)
			for i := 1; i <= 1_024; i++ {
				price := Price(i)
				if descending {
					price = Price(1_025 - i)
				}
				insertTestLevel(t, side, price)
				assertTreeInvariants(t, side)
			}
			for i := 1; i <= 1_024; i++ {
				price := Price(i)
				if descending {
					price = Price(1_025 - i)
				}
				if !side.removeLevel(side.levels[price]) {
					t.Fatalf("remove %d failed", price)
				}
				assertTreeInvariants(t, side)
			}
		})
	}
}

func TestTreeDifferentialAgainstMap(t *testing.T) {
	side := newTestSide(Buy)
	reference := make(map[Price]*priceLevel)
	rng := rand.New(rand.NewPCG(0xC0FFEE, 0xFACADE))

	for i := 0; i < 100_000; i++ {
		price := Price(rng.IntN(512) + 1)
		switch rng.IntN(3) {
		case 0:
			level := &priceLevel{price: price}
			inserted := side.insertLevel(level)
			_, exists := reference[price]
			if inserted == exists {
				t.Fatalf("step %d: insert price %d returned %v, exists=%v", i, price, inserted, exists)
			}
			if inserted {
				reference[price] = level
			}
		case 1:
			level := reference[price]
			removed := side.removeLevel(level)
			if removed != (level != nil) {
				t.Fatalf("step %d: remove price %d returned %v", i, price, removed)
			}
			if removed {
				delete(reference, price)
			}
		case 2:
			got, want := side.levels[price], reference[price]
			if got != want {
				t.Fatalf("step %d: lookup price %d got=%p want=%p", i, price, got, want)
			}
		}
		assertTreeInvariants(t, side)
	}
}

func FuzzTreeAgainstMap(f *testing.F) {
	f.Add([]byte{0, 10, 0, 5, 0, 15, 1, 10})
	f.Add([]byte{0, 1, 0, 2, 0, 3, 1, 2, 1, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		side := newTestSide(Buy)
		reference := make(map[Price]*priceLevel)
		if len(data) > 2_048 {
			data = data[:2_048]
		}
		for index := 0; index+1 < len(data); index += 2 {
			op, price := data[index]%3, Price(data[index+1])+1
			switch op {
			case 0:
				level := &priceLevel{price: price}
				inserted := side.insertLevel(level)
				_, exists := reference[price]
				if inserted == exists {
					t.Fatalf("insert price %d: inserted=%v exists=%v", price, inserted, exists)
				}
				if inserted {
					reference[price] = level
				}
			case 1:
				level := reference[price]
				removed := side.removeLevel(level)
				if removed != (level != nil) {
					t.Fatalf("remove price %d: removed=%v", price, removed)
				}
				if removed {
					delete(reference, price)
				}
			case 2:
				if side.levels[price] != reference[price] {
					t.Fatalf("lookup mismatch at price %d", price)
				}
			}
			assertTreeInvariants(t, side)
		}
	})
}

func BenchmarkTreeColdInsertDelete(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		side := newTestSide(Buy)
		level := &priceLevel{price: Price(i + 1)}
		side.insertLevel(level)
		side.removeLevel(level)
	}
}

func BenchmarkTreeWarmInsertDelete(b *testing.B) {
	side := newTestSide(Buy)
	level := &priceLevel{price: 1}
	side.insertLevel(level)
	side.removeLevel(level)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		*level = priceLevel{price: Price(i + 1)}
		if !side.insertLevel(level) {
			b.Fatal("insert failed")
		}
		if !side.removeLevel(level) {
			b.Fatal("remove failed")
		}
	}
}

func newTestSide(direction Side) *sideBook {
	side := &sideBook{}
	side.init(direction)
	return side
}

func insertTestLevel(t *testing.T, side *sideBook, price Price) *priceLevel {
	t.Helper()
	level := &priceLevel{price: price}
	if !side.insertLevel(level) {
		t.Fatalf("insert %d failed", price)
	}
	return level
}

func assertTreeInvariants(t *testing.T, side *sideBook) {
	t.Helper()
	if side.root == nil {
		if side.best != nil || len(side.levels) != 0 {
			t.Fatalf("empty tree inconsistent: best=%p levels=%d", side.best, len(side.levels))
		}
		return
	}
	if side.root.color != black {
		t.Fatal("root is not black")
	}
	prices := make([]Price, 0, len(side.levels))
	_, count := assertTreeNode(t, side, side.root, nil, nil, &prices)
	if count != len(side.levels) || len(prices) != len(side.levels) {
		t.Fatalf("tree count=%d traversal=%d map=%d", count, len(prices), len(side.levels))
	}
	for i := 1; i < len(prices); i++ {
		if prices[i-1] >= prices[i] {
			t.Fatalf("in-order prices are not strictly ascending: %d then %d", prices[i-1], prices[i])
		}
	}
	for _, price := range prices {
		if side.levels[price] == nil {
			t.Fatalf("tree price %d is missing from map", price)
		}
	}
	wantBest := minimum(side.root)
	if side.side == Buy {
		wantBest = maximum(side.root)
	}
	if side.best != wantBest {
		t.Fatalf("best=%p (%v), want=%p (%v)", side.best, priceOf(side.best), wantBest, priceOf(wantBest))
	}
}

func assertTreeNode(t *testing.T, side *sideBook, node *priceLevel, lower, upper *Price, prices *[]Price) (blackHeight, count int) {
	t.Helper()
	if node == nil {
		return 1, 0 // External NIL leaves are black.
	}
	if lower != nil && node.price <= *lower || upper != nil && node.price >= *upper {
		t.Fatalf("BST ordering violation at %d", node.price)
	}
	if side.levels[node.price] != node {
		t.Fatalf("map points to a different node for price %d", node.price)
	}
	if node.color == red && (colorOf(node.left) == red || colorOf(node.right) == red || colorOf(node.parent) == red) {
		t.Fatalf("red node %d has a red parent or child", node.price)
	}
	if node.left != nil && node.left.parent != node || node.right != nil && node.right.parent != node {
		t.Fatalf("broken parent-child link at %d", node.price)
	}
	price := node.price
	leftHeight, leftCount := assertTreeNode(t, side, node.left, lower, &price, prices)
	*prices = append(*prices, node.price)
	rightHeight, rightCount := assertTreeNode(t, side, node.right, &price, upper, prices)
	if leftHeight != rightHeight {
		t.Fatalf("black-height mismatch at %d: left=%d right=%d", node.price, leftHeight, rightHeight)
	}
	if node.color == black {
		leftHeight++
	}
	return leftHeight, leftCount + rightCount + 1
}

func priceOf(level *priceLevel) Price {
	if level == nil {
		return 0
	}
	return level.price
}
