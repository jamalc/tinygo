package marble

import "math/rand"

// The maze is carved on a coarse grid of cells, then rasterized to pixels. One
// cell is a cellSize×cellSize open square with a 1px wall on each side, so cells
// repeat every pitch pixels and a corridor is cellSize pixels wide — wide enough
// for the ball to have room to bounce around inside, narrow enough that a 64×32
// panel still holds a maze worth solving (15×7 cells).
const (
	cellSize = 3
	pitch    = cellSize + 1
)

// cell is a coordinate on the coarse maze grid.
type cell struct{ x, y int }

// maze is a carved maze, rasterized to the pixel grid the ball rolls on.
//
// walls is the rasterization and the only thing the physics consults: it is the
// full w×h panel, so the margin left over when the cell grid doesn't evenly
// divide the panel is wall too, and the ball simply cannot leave the board.
type maze struct {
	w, h       int
	cw, ch     int // size in cells
	offX, offY int // pixel offset of the maze rect, centering it on the panel
	walls      []bool
	// links is the carved maze as a cell graph: per cell, the neighbors it has a
	// passage to. The physics never reads it — walls is the ground truth there —
	// but the goal and hole placement are graph questions, not pixel ones.
	links [][]cell
	holes []cell
	start cell
	goal  cell
}

// newMaze carves a perfect maze (exactly one path between any two cells) on a
// w×h panel and drops holes in its dead ends. The goal is the cell farthest from
// the start, so solving it always means crossing the maze.
//
// holes is a request, not a promise: a maze has only so many dead ends, and the
// start, the goal, and the path's own dead ends are not eligible. Fewer are
// placed when that's all there's room for.
func newMaze(w, h, holes int, rng *rand.Rand) *maze {
	m := &maze{
		w:  w,
		h:  h,
		cw: (w - 1) / pitch,
		ch: (h - 1) / pitch,
	}
	// Center the leftover margin, so a panel that isn't a whole number of cells
	// wide is framed rather than lopsided.
	m.offX = (w - (m.cw*pitch + 1)) / 2
	m.offY = (h - (m.ch*pitch + 1)) / 2

	// Start with solid rock and carve passages out of it.
	m.walls = make([]bool, w*h)
	for i := range m.walls {
		m.walls[i] = true
	}

	m.start = cell{0, 0}
	m.carve(rng)
	m.goal = m.farthestFrom(m.start)
	m.placeHoles(holes, rng)
	return m
}

// carve runs a recursive backtracker (an iterative one — a device has no stack
// to spare) from the start cell, opening a passage into each unvisited neighbor
// it wanders into. Every cell is reached exactly once, which is what makes the
// maze perfect.
//
// It fills in m.links as it goes, which is the adjacency the goal and hole
// placement then walk.
func (m *maze) carve(rng *rand.Rand) {
	m.links = make([][]cell, m.cw*m.ch)
	visited := make([]bool, m.cw*m.ch)

	m.hollow(m.start)
	visited[m.index(m.start)] = true
	stack := []cell{m.start}

	for len(stack) > 0 {
		cur := stack[len(stack)-1]

		unvisited := make([]cell, 0, 4)
		for _, n := range m.neighbors(cur) {
			if !visited[m.index(n)] {
				unvisited = append(unvisited, n)
			}
		}
		if len(unvisited) == 0 {
			stack = stack[:len(stack)-1] // dead end: back up
			continue
		}

		next := unvisited[rng.Intn(len(unvisited))]
		m.hollow(next)
		m.join(cur, next)
		m.links[m.index(cur)] = append(m.links[m.index(cur)], next)
		m.links[m.index(next)] = append(m.links[m.index(next)], cur)
		visited[m.index(next)] = true
		stack = append(stack, next)
	}
}

// farthestFrom returns the cell at the greatest distance from src, walking the
// carved passages. A breadth-first search settles ties toward the cell found
// first, which is good enough: any farthest cell makes a fair goal.
func (m *maze) farthestFrom(src cell) cell {
	dist := make([]int, m.cw*m.ch)
	for i := range dist {
		dist[i] = -1
	}
	dist[m.index(src)] = 0
	queue := []cell{src}
	far := src

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if dist[m.index(cur)] > dist[m.index(far)] {
			far = cur
		}
		for _, n := range m.links[m.index(cur)] {
			if dist[m.index(n)] < 0 {
				dist[m.index(n)] = dist[m.index(cur)] + 1
				queue = append(queue, n)
			}
		}
	}
	return far
}

// placeHoles drops up to n holes into dead ends — cells with a single passage,
// which are the cells a player can only enter by making a mistake. The start and
// the goal are spared, and so is any dead end on the solution path, so a maze is
// never unsolvable.
//
// Punishing only dead ends is what makes the game about control rather than
// luck: a hole is always somewhere you had the option not to go.
func (m *maze) placeHoles(n int, rng *rand.Rand) {
	onPath := m.solution()

	candidates := make([]cell, 0, m.cw*m.ch)
	for y := 0; y < m.ch; y++ {
		for x := 0; x < m.cw; x++ {
			c := cell{x, y}
			if len(m.links[m.index(c)]) != 1 || onPath[m.index(c)] {
				continue
			}
			candidates = append(candidates, c)
		}
	}

	rng.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})
	if n > len(candidates) {
		n = len(candidates)
	}
	m.holes = candidates[:n]
}

// solution marks the cells on the one path from start to goal. In a perfect maze
// that path is unique, so walking parents back from the goal recovers it exactly.
func (m *maze) solution() []bool {
	parent := make([]cell, m.cw*m.ch)
	seen := make([]bool, m.cw*m.ch)
	seen[m.index(m.start)] = true
	queue := []cell{m.start}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, n := range m.links[m.index(cur)] {
			if !seen[m.index(n)] {
				seen[m.index(n)] = true
				parent[m.index(n)] = cur
				queue = append(queue, n)
			}
		}
	}

	onPath := make([]bool, m.cw*m.ch)
	for c := m.goal; c != m.start; c = parent[m.index(c)] {
		onPath[m.index(c)] = true
	}
	onPath[m.index(m.start)] = true
	return onPath
}

// neighbors returns the cells adjacent to c on the coarse grid.
func (m *maze) neighbors(c cell) []cell {
	all := []cell{{c.x, c.y - 1}, {c.x, c.y + 1}, {c.x - 1, c.y}, {c.x + 1, c.y}}
	in := make([]cell, 0, 4)
	for _, n := range all {
		if n.x >= 0 && n.x < m.cw && n.y >= 0 && n.y < m.ch {
			in = append(in, n)
		}
	}
	return in
}

// index is c's slot in the per-cell slices.
func (m *maze) index(c cell) int { return c.y*m.cw + c.x }

// corner is the pixel just inside c's top-left wall.
func (m *maze) corner(c cell) (int, int) {
	return m.offX + c.x*pitch + 1, m.offY + c.y*pitch + 1
}

// center is the pixel at the middle of c, where the ball and the markers sit.
func (m *maze) center(c cell) (int, int) {
	x, y := m.corner(c)
	return x + cellSize/2, y + cellSize/2
}

// hollow carves out c's interior, leaving the walls around it standing. join is
// what then knocks a hole in one of those walls.
func (m *maze) hollow(c cell) {
	x0, y0 := m.corner(c)
	for y := y0; y < y0+cellSize; y++ {
		for x := x0; x < x0+cellSize; x++ {
			m.walls[y*m.w+x] = false
		}
	}
}

// join knocks out the wall between two adjacent cells, leaving a passage the
// full width of a corridor.
func (m *maze) join(a, b cell) {
	ax, ay := m.corner(a)
	bx, by := m.corner(b)
	switch {
	case a.x != b.x: // side by side: clear the wall column between them
		x := max(ax, bx) - 1
		for y := ay; y < ay+cellSize; y++ {
			m.walls[y*m.w+x] = false
		}
	default: // stacked: clear the wall row between them
		y := max(ay, by) - 1
		for x := ax; x < ax+cellSize; x++ {
			m.walls[y*m.w+x] = false
		}
	}
}

// wallAt reports whether the pixel is wall. Everything off the panel is, so the
// physics gets its outer boundary for free.
func (m *maze) wallAt(x, y int) bool {
	if x < 0 || x >= m.w || y < 0 || y >= m.h {
		return true
	}
	return m.walls[y*m.w+x]
}
