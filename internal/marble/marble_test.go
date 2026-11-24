package marble

import (
	"math/rand"
	"testing"

	"github.com/jamalc/tinygo/internal/input"
)

// level is a maze with no holes, so a physics test can run for as long as it
// likes without the ball falling down one.
func level(seed int64) *game {
	return newGame(newMaze(panelW, panelH, 0, rand.New(rand.NewSource(seed))))
}

func TestBallStartsAtRestOnTheStart(t *testing.T) {
	g := level(1)

	sx, sy := g.m.center(g.m.start)
	if round(g.x) != sx || round(g.y) != sy {
		t.Errorf("ball at (%d, %d), want it on the start cell (%d, %d)", round(g.x), round(g.y), sx, sy)
	}
	if g.vx != 0 || g.vy != 0 {
		t.Errorf("ball velocity = (%v, %v), want it at rest", g.vx, g.vy)
	}
}

func TestALevelBoardLetsTheBallSettle(t *testing.T) {
	g := level(1)
	g.vx, g.vy = 0.9, 0.9

	// Drag alone must bring a rolling ball to a stop when the player levels the
	// board, or nothing would ever be controllable.
	for i := 0; i < 400; i++ {
		g.step(input.Tilt{})
	}
	if absf(g.vx) > 0.01 || absf(g.vy) > 0.01 {
		t.Errorf("after 400 level frames the ball is still moving at (%v, %v), want it settled", g.vx, g.vy)
	}
}

func TestLeanAcceleratesTheBallDownhill(t *testing.T) {
	g := level(1)

	// Lean east. The ball must pick up eastward speed — and it's in the start
	// cell's interior, so no wall gets in the way of the first few frames.
	for i := 0; i < 3; i++ {
		g.step(input.Tilt{X: 1})
	}
	if g.vx <= 0 {
		t.Errorf("vx = %v after leaning east, want the ball rolling east", g.vx)
	}
	if g.vy != 0 {
		t.Errorf("vy = %v after a purely eastward lean, want no north/south motion", g.vy)
	}
}

func TestBounceReversesAndDampens(t *testing.T) {
	g := level(1)
	// Hard west, into the maze's outer boundary. The start cell is the top-left
	// one, so the wall on its west side is the edge of the board — the one wall
	// that is never carved through, whatever the maze generator rolled.
	g.vx = -5

	g.step(input.Tilt{})

	if g.vx <= 0 {
		t.Fatalf("vx = %v after hitting a wall, want it reversed", g.vx)
	}
	if absf(g.vx) >= 5 {
		t.Errorf("|vx| = %v after the bounce, want the wall to have taken energy out of it", absf(g.vx))
	}
}

func TestBallNeverEntersAWall(t *testing.T) {
	// The tunneling test, and the reason step() substeps at all. A ball driven
	// as hard as the physics allows, for a long time, on several mazes, must
	// never once end a frame inside a wall pixel or off the panel.
	rng := rand.New(rand.NewSource(7))
	for seed := int64(0); seed < 5; seed++ {
		g := level(seed)
		for i := 0; i < 5000; i++ {
			// A lean that flips around at random is far more violent than a
			// human hand, which makes it exactly the input worth testing.
			g.step(input.Tilt{X: rng.Float32()*2 - 1, Y: rng.Float32()*2 - 1})

			x, y := round(g.x), round(g.y)
			if g.m.wallAt(x, y) {
				t.Fatalf("seed %d, frame %d: the ball is inside a wall at (%d, %d), moving (%v, %v)",
					seed, i, x, y, g.vx, g.vy)
			}
		}
	}
}

func TestReachingTheGoalWins(t *testing.T) {
	g := level(1)

	gx, gy := g.m.center(g.m.goal)
	g.x, g.y = float32(gx), float32(gy)

	if got := g.step(input.Tilt{}); got != reached {
		t.Errorf("step() with the ball on the goal = %v, want reached", got)
	}
}

func TestRollingOverAHoleFalls(t *testing.T) {
	m := newMaze(panelW, panelH, 5, rand.New(rand.NewSource(1)))
	if len(m.holes) == 0 {
		t.Fatal("the maze has no holes to test against")
	}
	g := newGame(m)

	hx, hy := m.center(m.holes[0])
	g.x, g.y = float32(hx), float32(hy)

	if got := g.step(input.Tilt{}); got != fell {
		t.Errorf("step() with the ball on a hole = %v, want fell", got)
	}
}

func TestFastBallDoesNotSkipOverAHole(t *testing.T) {
	// The holes are checked every substep, not once a frame, so a ball crossing
	// a hole at speed drops into it rather than flying over.
	m := newMaze(panelW, panelH, 5, rand.New(rand.NewSource(1)))
	if len(m.holes) == 0 {
		t.Fatal("the maze has no holes to test against")
	}
	g := newGame(m)

	// Fire the ball down the hole's own corridor: from the middle of the one
	// cell that opens onto it, straight at it. A hole sits in a dead end, so
	// that corridor is the only clear line of approach.
	h := m.holes[0]
	n := m.links[m.index(h)][0]
	hx, hy := m.center(h)
	nx, ny := m.center(n)
	g.x, g.y = float32(nx), float32(ny)

	// Fast enough that one unsubstepped move would carry the ball a full pixel
	// past the hole's mouth and out the far side of it, missing entirely.
	const speed = 6
	g.vx = speed * sign(hx-nx)
	g.vy = speed * sign(hy-ny)

	if got := g.step(input.Tilt{}); got != fell {
		t.Errorf("step() for a ball crossing a hole at speed = %v, want fell", got)
	}
}

func sign(n int) float32 {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	}
	return 0
}

func TestResetReturnsTheBallToTheStart(t *testing.T) {
	g := level(1)
	g.x, g.y, g.vx, g.vy = 30, 20, 1.5, -2

	g.reset()

	sx, sy := g.m.center(g.m.start)
	if round(g.x) != sx || round(g.y) != sy {
		t.Errorf("ball at (%d, %d) after reset, want it back on the start (%d, %d)",
			round(g.x), round(g.y), sx, sy)
	}
	if g.vx != 0 || g.vy != 0 {
		t.Errorf("velocity = (%v, %v) after reset, want the ball at rest", g.vx, g.vy)
	}
}
