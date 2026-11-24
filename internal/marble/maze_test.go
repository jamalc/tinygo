package marble

import (
	"math/rand"
	"testing"
)

// The panel the game actually ships on. The maze code is size-agnostic, but the
// properties are worth asserting at the real size.
const (
	panelW = 64
	panelH = 32
)

func TestMazeFitsThePanel(t *testing.T) {
	m := newMaze(panelW, panelH, 5, rand.New(rand.NewSource(1)))

	if m.cw != 15 || m.ch != 7 {
		t.Errorf("maze is %d×%d cells, want 15×7 on a %d×%d panel", m.cw, m.ch, panelW, panelH)
	}
	// Every pixel of the leftover margin must be wall, or the ball could roll
	// out of the maze and into the gap.
	for y := 0; y < panelH; y++ {
		for x := 0; x < panelW; x++ {
			inRect := x >= m.offX && x <= m.offX+m.cw*pitch &&
				y >= m.offY && y <= m.offY+m.ch*pitch
			if !inRect && !m.wallAt(x, y) {
				t.Fatalf("pixel (%d, %d) is outside the maze but is not wall", x, y)
			}
		}
	}
}

func TestWallAtIsSolidOffThePanel(t *testing.T) {
	m := newMaze(panelW, panelH, 0, rand.New(rand.NewSource(1)))

	for _, p := range [][2]int{{-1, 5}, {panelW, 5}, {5, -1}, {5, panelH}} {
		if !m.wallAt(p[0], p[1]) {
			t.Errorf("wallAt(%d, %d) off the panel = false, want the board to be walled in", p[0], p[1])
		}
	}
}

// passable reports whether the ball can actually roll from a to b, asking the
// pixel grid rather than the cell graph. Adjacent cell centers are pitch apart,
// so the pixel exactly between them is the wall that separates them.
func passable(m *maze, a, b cell) bool {
	ax, ay := m.center(a)
	bx, by := m.center(b)
	return !m.wallAt((ax+bx)/2, (ay+by)/2)
}

// reachable returns the cells reachable from the start by rolling through the
// carved pixels — the maze as the ball experiences it. The connectivity tests go
// through this rather than through m.links, so a bug in the rasterization can't
// hide behind a correct cell graph.
func reachable(m *maze) map[cell]bool {
	seen := map[cell]bool{m.start: true}
	queue := []cell{m.start}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		for _, n := range m.neighbors(c) {
			if seen[n] || !passable(m, c, n) {
				continue
			}
			seen[n] = true
			queue = append(queue, n)
		}
	}
	return seen
}

func TestMazeIsFullyConnected(t *testing.T) {
	// A perfect maze reaches every cell. Several seeds, since carving is random.
	for seed := int64(0); seed < 20; seed++ {
		m := newMaze(panelW, panelH, 5, rand.New(rand.NewSource(seed)))
		if got, want := len(reachable(m)), m.cw*m.ch; got != want {
			t.Errorf("seed %d: %d of %d cells reachable from the start, want every one", seed, got, want)
		}
	}
}

func TestGoalIsReachableAndNotTheStart(t *testing.T) {
	for seed := int64(0); seed < 20; seed++ {
		m := newMaze(panelW, panelH, 5, rand.New(rand.NewSource(seed)))
		if m.goal == m.start {
			t.Errorf("seed %d: the goal is the start cell — the level is already won", seed)
		}
		if !reachable(m)[m.goal] {
			t.Errorf("seed %d: the goal at %v cannot be reached from the start", seed, m.goal)
		}
	}
}

func TestHolesNeverBlockTheSolution(t *testing.T) {
	for seed := int64(0); seed < 20; seed++ {
		m := newMaze(panelW, panelH, maxHoles, rand.New(rand.NewSource(seed)))

		onPath := m.solution()
		for _, hole := range m.holes {
			if hole == m.start || hole == m.goal {
				t.Errorf("seed %d: a hole was placed on the start or the goal at %v", seed, hole)
			}
			if onPath[m.index(hole)] {
				t.Errorf("seed %d: a hole at %v sits on the only path to the goal", seed, hole)
			}
		}
	}
}

func TestHolesOnlySitInDeadEnds(t *testing.T) {
	// A hole must always be somewhere the player chose to go, never somewhere
	// they were merely passing through.
	for seed := int64(0); seed < 20; seed++ {
		m := newMaze(panelW, panelH, maxHoles, rand.New(rand.NewSource(seed)))
		for _, hole := range m.holes {
			if n := len(m.links[m.index(hole)]); n != 1 {
				t.Errorf("seed %d: a hole at %v has %d passages, want a dead end (1)", seed, hole, n)
			}
		}
	}
}

func TestHolesAreRequestedNotPromised(t *testing.T) {
	m := newMaze(panelW, panelH, 1000, rand.New(rand.NewSource(1)))

	if len(m.holes) == 0 {
		t.Fatal("asked for more holes than the maze has dead ends and got none at all")
	}
	if len(m.holes) >= m.cw*m.ch {
		t.Errorf("placed %d holes in a %d-cell maze — more than its dead ends can hold",
			len(m.holes), m.cw*m.ch)
	}
}

func TestHolesFor(t *testing.T) {
	if got := holesFor(1); got != baseHoles {
		t.Errorf("holesFor(1) = %d, want %d", got, baseHoles)
	}
	if got, want := holesFor(3), baseHoles+2*holesPer; got != want {
		t.Errorf("holesFor(3) = %d, want %d (each level adds holes)", got, want)
	}
	if got := holesFor(100); got != maxHoles {
		t.Errorf("holesFor(100) = %d, want it capped at %d", got, maxHoles)
	}
}
