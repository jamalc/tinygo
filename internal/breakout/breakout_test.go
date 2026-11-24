package breakout

import (
	"math"
	"math/rand"
	"testing"

	"github.com/jamalc/tinygo/internal/input"
)

// The panel the game actually ships on.
const (
	panelW = 64
	panelH = 32
)

func TestNewGameRacksAFullWall(t *testing.T) {
	g := newGame(panelW, panelH)

	if got, want := g.standing(), g.cols*brickRows; got != want {
		t.Errorf("%d bricks standing at the start, want a full wall of %d", got, want)
	}
	if g.cols != 8 {
		t.Errorf("wall is %d bricks wide, want 8 on a %d px panel", g.cols, panelW)
	}
	if g.lives != startLives {
		t.Errorf("lives = %d, want %d", g.lives, startLives)
	}
}

func TestServeWaitsOnThePaddle(t *testing.T) {
	g := newGame(panelW, panelH)

	if !g.serving {
		t.Fatal("a fresh game isn't serving, want the ball waiting for the press")
	}
	if round(g.y) != g.paddleRow()-1 {
		t.Errorf("waiting ball on row %d, want it sitting on the paddle at %d", round(g.y), g.paddleRow()-1)
	}

	// Un-pressed, the ball stays put and rides the paddle as it moves.
	for i := 0; i < 20; i++ {
		g.step(input.Tilt{X: 1}, false)
	}
	if !g.serving {
		t.Fatal("the serve launched without a press, long before the grace ran out")
	}
	if want := g.paddleX + (paddleW-1)/2; g.x != want {
		t.Errorf("waiting ball at x=%v, want it riding the moving paddle's center at %v", g.x, want)
	}
}

func TestPressLaunchesTheServeHeadingUp(t *testing.T) {
	g := newGame(panelW, panelH)

	g.step(input.Tilt{}, true)

	if g.serving {
		t.Fatal("still serving after the press, want the ball launched")
	}
	if g.vy >= 0 {
		t.Errorf("vy = %v after the launch, want the ball heading up the panel", g.vy)
	}
	// The launched ball's speed is the level's speed, whichever way it's aimed.
	if got, want := hypot(g.vx, g.vy), g.speed(); absf(got-want) > 0.001 {
		t.Errorf("launched ball's speed = %v, want %v", got, want)
	}
}

func TestServeGraceLaunchesByItself(t *testing.T) {
	// A src that never fires (a board with no button at all) still gets a game.
	g := newGame(panelW, panelH)

	for i := 0; i < serveGrace+1 && g.serving; i++ {
		g.step(input.Tilt{}, false)
	}
	if g.serving {
		t.Fatal("the serve never launched itself, want it served once the grace ran out")
	}
}

func TestPaddleFollowsTheLeanAndStopsAtTheWalls(t *testing.T) {
	g := newGame(panelW, panelH)
	start := g.paddleX

	for i := 0; i < 10; i++ {
		g.movePaddle(input.Tilt{X: 1})
	}
	if g.paddleX <= start {
		t.Errorf("paddle at %v after leaning right, want it further right than %v", g.paddleX, start)
	}

	// Lean into the wall for far longer than it takes to get there.
	for i := 0; i < 400; i++ {
		g.movePaddle(input.Tilt{X: 1})
	}
	if want := float32(panelW - paddleW); g.paddleX != want {
		t.Errorf("paddle at %v against the right wall, want it stopped at %v", g.paddleX, want)
	}
	if g.paddleV != 0 {
		t.Errorf("paddle jammed against the wall still has speed %v, want it dead — otherwise it "+
			"springs away the moment the lean eases", g.paddleV)
	}
}

func TestLevelBoardCoastsThePaddleToAStop(t *testing.T) {
	g := newGame(panelW, panelH)
	g.paddleV = paddleMax

	for i := 0; i < 200; i++ {
		g.movePaddle(input.Tilt{})
	}
	if absf(g.paddleV) > 0.01 {
		t.Errorf("paddle still moving at %v after the board was levelled, want it coasted to a stop", g.paddleV)
	}
}

func TestBallBreaksABrickAndScores(t *testing.T) {
	g := newGame(panelW, panelH)
	before := g.standing()

	// Park the ball just under the bottom-left brick and send it up into it.
	g.launch()
	g.x, g.y = 3, float32(brickTop+(brickRows-1)*brickPitchY+brickH)
	g.vx, g.vy = 0, -g.speed()

	for i := 0; i < 10 && g.standing() == before; i++ {
		g.step(input.Tilt{}, false)
	}

	if g.standing() != before-1 {
		t.Fatalf("%d bricks standing after the ball hit the wall, want %d", g.standing(), before-1)
	}
	if g.score != 1 {
		t.Errorf("score = %d, want 1", g.score)
	}
	if g.vy <= 0 {
		t.Errorf("vy = %v after hitting a brick from below, want the ball sent back down", g.vy)
	}
}

func TestMortarIsNotABrick(t *testing.T) {
	g := newGame(panelW, panelH)

	// The 8th pixel of a brick's pitch is the mortar beside it, and the 3rd row
	// of the pitch is the mortar under it. Neither belongs to a brick.
	if i := g.brickAt(brickW, brickTop); i >= 0 {
		t.Errorf("the pixel beside a brick reports brick %d, want mortar", i)
	}
	if i := g.brickAt(0, brickTop+brickH); i >= 0 {
		t.Errorf("the pixel under a brick reports brick %d, want mortar", i)
	}
	if i := g.brickAt(0, brickTop); i < 0 {
		t.Error("the top-left pixel of the wall reports mortar, want a brick")
	}
}

func TestBrokenBricksStayBroken(t *testing.T) {
	g := newGame(panelW, panelH)

	if i := g.brickAt(0, brickTop); i >= 0 {
		g.bricks[i] = false
	}
	if i := g.brickAt(0, brickTop); i >= 0 {
		t.Errorf("a broken brick's pixel still reports brick %d, want the ball to fly through it", i)
	}
}

func TestPaddleSteersTheBounce(t *testing.T) {
	tests := []struct {
		name string
		// where the ball lands, as a fraction of the paddle's half-width
		at   float32
		want string
	}{
		{"the left tip throws the ball left", -1, "left"},
		{"the middle sends the ball straight back up", 0, "straight"},
		{"the right tip throws the ball right", 1, "right"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newGame(panelW, panelH)
			center := g.paddleX + (paddleW-1)/2
			g.x = center + tc.at*((paddleW-1)/2)
			g.y = float32(g.paddleRow())
			g.vx, g.vy = 0, g.speed()

			g.bounceOffPaddle()

			if g.vy >= 0 {
				t.Fatalf("vy = %v after the save, want the ball sent back up", g.vy)
			}
			switch tc.want {
			case "left":
				if g.vx >= 0 {
					t.Errorf("vx = %v off the left tip, want the ball thrown left", g.vx)
				}
			case "right":
				if g.vx <= 0 {
					t.Errorf("vx = %v off the right tip, want the ball thrown right", g.vx)
				}
			case "straight":
				if absf(g.vx) > 0.001 {
					t.Errorf("vx = %v off the middle, want the ball sent straight back up", g.vx)
				}
			}
			// However it's steered, a bounce never changes the ball's speed.
			if got, want := hypot(g.vx, g.vy), g.speed(); absf(got-want) > 0.001 {
				t.Errorf("speed = %v after the bounce, want it unchanged at %v", got, want)
			}
		})
	}
}

func TestBounceAlwaysKeepsTheBallClimbing(t *testing.T) {
	// steer is capped below 1 so a bounce can never flatten the ball out into a
	// purely horizontal path, which it would never escape.
	g := newGame(panelW, panelH)
	for _, at := range []float32{-2, -1, 0, 1, 2} { // including off the ends, clamped
		center := g.paddleX + (paddleW-1)/2
		g.x = center + at*((paddleW-1)/2)
		g.x = clampf(g.x, g.paddleX, g.paddleX+paddleW-1)
		g.y = float32(g.paddleRow())
		g.vx, g.vy = 0, g.speed()

		g.bounceOffPaddle()

		if g.vy > -0.2*g.speed() {
			t.Errorf("landing at %v: vy = %v, want the ball still climbing meaningfully", at, g.vy)
		}
	}
}

func TestBallPastTheEndOfThePaddleIsNotSaved(t *testing.T) {
	g := newGame(panelW, panelH)
	g.x = g.paddleX + paddleW + 1 // just off the right end
	g.y = float32(g.paddleRow())
	g.vx, g.vy = 0, g.speed()

	g.bounceOffPaddle()

	if g.vy <= 0 {
		t.Errorf("vy = %v for a ball past the end of the paddle, want it still falling", g.vy)
	}
}

func TestBallFallingPastThePaddleIsLost(t *testing.T) {
	g := newGame(panelW, panelH)
	g.launch()
	g.x = 2                          // far from the paddle, which sits centered
	g.y = float32(g.paddleRow()) + 1 // already below it
	g.vx, g.vy = 0, g.speed()

	for i := 0; i < 30; i++ {
		if g.step(input.Tilt{}, false) == lost {
			return
		}
	}
	t.Errorf("the ball fell past the paddle and was never lost (it ended at y=%v)", g.y)
}

func TestClearingTheWallClearsTheLevel(t *testing.T) {
	g := newGame(panelW, panelH)
	g.launch()
	for i := range g.bricks {
		g.bricks[i] = false
	}

	if got := g.step(input.Tilt{}, false); got != cleared {
		t.Errorf("step() with the wall gone = %v, want cleared", got)
	}
}

func TestSpeedRisesPerLevelAndIsCapped(t *testing.T) {
	g := newGame(panelW, panelH)

	if got := g.speed(); got != baseSpeed {
		t.Errorf("speed at level 1 = %v, want %v", got, baseSpeed)
	}
	g.level = 3
	if got, want := g.speed(), float32(baseSpeed+2*speedPer); absf(got-want) > 0.001 {
		t.Errorf("speed at level 3 = %v, want %v", got, want)
	}
	g.level = 100
	if got := g.speed(); got != maxSpeed {
		t.Errorf("speed at level 100 = %v, want it capped at %v", got, maxSpeed)
	}
}

func TestBallNeverEscapesThePanel(t *testing.T) {
	// The tunneling test, and the reason the ball substeps. Play a long game at
	// the fastest the ball ever goes, with the paddle thrashing, and the ball
	// must never leave through a wall or the ceiling — only ever through the
	// floor, which is a lost life rather than an escape.
	rng := rand.New(rand.NewSource(3))
	g := newGame(panelW, panelH)
	g.level = 100 // maxSpeed
	g.serve()
	g.launch()

	for i := 0; i < 20000; i++ {
		// Launch every serve immediately: this test is about the ball in
		// flight, and a waiting ball covers no ground.
		switch g.step(input.Tilt{X: rng.Float32()*2 - 1}, false) {
		case lost:
			g.serve()
			g.launch()
		case cleared:
			g.rack()
			g.launch()
		}

		if x := round(g.x); x < 0 || x >= panelW {
			t.Fatalf("frame %d: the ball left the panel sideways at x=%d", i, x)
		}
		if y := round(g.y); y < 0 {
			t.Fatalf("frame %d: the ball went through the ceiling at y=%d", i, y)
		}
	}
}

func hypot(x, y float32) float32 {
	return float32(math.Sqrt(float64(x*x + y*y)))
}
