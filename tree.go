package orderbook

type order struct {
	id         uint64
	requestID  uint64
	side       Side
	price      Price
	remaining  Quantity
	sequence   uint64
	acceptedAt int64
	level      *priceLevel
	prev, next *order
	freeNext   *order
}

type rbColor bool

const (
	red   rbColor = false
	black rbColor = true
)

type priceLevel struct {
	price Price
	total Quantity
	head  *order
	tail  *order

	color               rbColor
	parent, left, right *priceLevel
	freeNext            *priceLevel
}

type sideBook struct {
	side   Side
	levels map[Price]*priceLevel
	root   *priceLevel
	best   *priceLevel
}

func (s *sideBook) init(side Side) {
	s.side = side
	s.levels = make(map[Price]*priceLevel)
}

func (s *sideBook) insertLevel(level *priceLevel) bool {
	if level == nil {
		return false
	}
	if _, exists := s.levels[level.price]; exists {
		return false
	}
	var parent *priceLevel
	node := s.root
	for node != nil {
		parent = node
		if level.price < node.price {
			node = node.left
		} else {
			node = node.right
		}
	}
	level.parent = parent
	level.color = red
	if parent == nil {
		s.root = level
	} else if level.price < parent.price {
		parent.left = level
	} else {
		parent.right = level
	}
	s.insertFix(level)
	s.levels[level.price] = level
	if s.best == nil || s.better(level.price, s.best.price) {
		s.best = level
	}
	return true
}

func (s *sideBook) removeLevel(level *priceLevel) bool {
	if level == nil || s.levels[level.price] != level {
		return false
	}
	delete(s.levels, level.price)
	s.deleteNode(level)
	if s.best == level {
		if s.side == Buy {
			s.best = maximum(s.root)
		} else {
			s.best = minimum(s.root)
		}
	}
	return true
}

func (s *sideBook) better(a, b Price) bool {
	if s.side == Buy {
		return a > b
	}
	return a < b
}

func colorOf(node *priceLevel) rbColor {
	if node == nil {
		return black
	}
	return node.color
}

func minimum(node *priceLevel) *priceLevel {
	for node != nil && node.left != nil {
		node = node.left
	}
	return node
}

func maximum(node *priceLevel) *priceLevel {
	for node != nil && node.right != nil {
		node = node.right
	}
	return node
}

func (s *sideBook) rotateLeft(node *priceLevel) {
	right := node.right
	node.right = right.left
	if right.left != nil {
		right.left.parent = node
	}
	right.parent = node.parent
	if node.parent == nil {
		s.root = right
	} else if node == node.parent.left {
		node.parent.left = right
	} else {
		node.parent.right = right
	}
	right.left = node
	node.parent = right
}

func (s *sideBook) rotateRight(node *priceLevel) {
	left := node.left
	node.left = left.right
	if left.right != nil {
		left.right.parent = node
	}
	left.parent = node.parent
	if node.parent == nil {
		s.root = left
	} else if node == node.parent.right {
		node.parent.right = left
	} else {
		node.parent.left = left
	}
	left.right = node
	node.parent = left
}

func (s *sideBook) insertFix(node *priceLevel) {
	for node != s.root && colorOf(node.parent) == red {
		parent := node.parent
		grand := parent.parent
		if parent == grand.left {
			uncle := grand.right
			if colorOf(uncle) == red {
				parent.color = black
				uncle.color = black
				grand.color = red
				node = grand
			} else {
				if node == parent.right {
					node = parent
					s.rotateLeft(node)
					parent = node.parent
					grand = parent.parent
				}
				parent.color = black
				grand.color = red
				s.rotateRight(grand)
			}
		} else {
			uncle := grand.left
			if colorOf(uncle) == red {
				parent.color = black
				uncle.color = black
				grand.color = red
				node = grand
			} else {
				if node == parent.left {
					node = parent
					s.rotateRight(node)
					parent = node.parent
					grand = parent.parent
				}
				parent.color = black
				grand.color = red
				s.rotateLeft(grand)
			}
		}
	}
	s.root.color = black
}

func (s *sideBook) transplant(old, replacement *priceLevel) {
	if old.parent == nil {
		s.root = replacement
	} else if old == old.parent.left {
		old.parent.left = replacement
	} else {
		old.parent.right = replacement
	}
	if replacement != nil {
		replacement.parent = old.parent
	}
}

func (s *sideBook) deleteNode(node *priceLevel) {
	y := node
	yOriginal := y.color
	var x, xParent *priceLevel
	if node.left == nil {
		x = node.right
		xParent = node.parent
		s.transplant(node, node.right)
	} else if node.right == nil {
		x = node.left
		xParent = node.parent
		s.transplant(node, node.left)
	} else {
		y = minimum(node.right)
		yOriginal = y.color
		x = y.right
		if y.parent == node {
			xParent = y
			if x != nil {
				x.parent = y
			}
		} else {
			xParent = y.parent
			s.transplant(y, y.right)
			y.right = node.right
			y.right.parent = y
		}
		s.transplant(node, y)
		y.left = node.left
		y.left.parent = y
		y.color = node.color
	}
	if yOriginal == black {
		s.deleteFix(x, xParent)
	}
	node.parent, node.left, node.right = nil, nil, nil
}

func (s *sideBook) deleteFix(node, parent *priceLevel) {
	for node != s.root && colorOf(node) == black {
		if parent == nil {
			break
		}
		if node == parent.left {
			sibling := parent.right
			if colorOf(sibling) == red {
				sibling.color = black
				parent.color = red
				s.rotateLeft(parent)
				sibling = parent.right
			}
			if colorOf(leftOf(sibling)) == black && colorOf(rightOf(sibling)) == black {
				if sibling != nil {
					sibling.color = red
				}
				node = parent
				parent = node.parent
			} else {
				if colorOf(rightOf(sibling)) == black {
					if left := leftOf(sibling); left != nil {
						left.color = black
					}
					if sibling != nil {
						sibling.color = red
						s.rotateRight(sibling)
					}
					sibling = parent.right
				}
				if sibling != nil {
					sibling.color = parent.color
				}
				parent.color = black
				if right := rightOf(sibling); right != nil {
					right.color = black
				}
				s.rotateLeft(parent)
				node = s.root
				parent = nil
			}
		} else {
			sibling := parent.left
			if colorOf(sibling) == red {
				sibling.color = black
				parent.color = red
				s.rotateRight(parent)
				sibling = parent.left
			}
			if colorOf(rightOf(sibling)) == black && colorOf(leftOf(sibling)) == black {
				if sibling != nil {
					sibling.color = red
				}
				node = parent
				parent = node.parent
			} else {
				if colorOf(leftOf(sibling)) == black {
					if right := rightOf(sibling); right != nil {
						right.color = black
					}
					if sibling != nil {
						sibling.color = red
						s.rotateLeft(sibling)
					}
					sibling = parent.left
				}
				if sibling != nil {
					sibling.color = parent.color
				}
				parent.color = black
				if left := leftOf(sibling); left != nil {
					left.color = black
				}
				s.rotateRight(parent)
				node = s.root
				parent = nil
			}
		}
	}
	if node != nil {
		node.color = black
	}
}

func leftOf(node *priceLevel) *priceLevel {
	if node == nil {
		return nil
	}
	return node.left
}

func rightOf(node *priceLevel) *priceLevel {
	if node == nil {
		return nil
	}
	return node.right
}
