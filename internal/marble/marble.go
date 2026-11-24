// Package marble runs a tilt maze on a HUB75-style display: a ball rolls under
// gravity through a carved maze, falling into holes if you're careless, and the
// board's lean is the gravity.
//
// The game takes the lean itself (input.TiltSource) and integrates it, rather
// than quantizing it into four directions. That's the whole point — the ball
// carries momentum, so it overshoots corners and rebounds off walls, and getting
// it to stop where you want is the game. Quantizing the tilt to four directions
// first would flatten exactly the thing being played.
//
// Like the other apps it drives any Display, so the same code runs on the panel
// and in the host simulator.
package marble

import (
	"image/color"
	"math"
	"math/rand"
	"strconv"
	"time"

	"tinygo.org/x/tinyfont"

	"github.com/jamalc/tinygo/internal/config"
	"github.com/jamalc/tinygo/internal/input"
)

// Display is the subset of a panel the game drives. Both hub75.Device and
// simulator.Display satisfy it. Size is here because tinyfont needs it to write
// the level text.
type Display interface {
	SetPixel(x, y int16, c color.RGBA)
	Size() (x, y int16)
	Display() error
	Clear()
}

// Physics, in pixels and frames. The panel is small enough that the honest
// units are pixels per frame; there's no point pretending to SI.
const (
	// gravity is the acceleration at full lean. With drag, it settles the ball
	// at a top speed of about gravity/(1-drag) ≈ 1.2 px/frame — brisk enough to
	// cross the panel in a second, slow enough to steer.
	gravity = 0.075
	// drag bleeds speed every frame, so the ball settles instead of rattling
	// around forever, and a centered board brings it to rest.
	drag = 0.94
	// restitution is the fraction of speed kept in a bounce. Low: walls are
	// concrete, not rubber. A lively wall would make the maze unplayable.
	restitution = 0.35
	// substep caps how far the ball may move between wall checks. Walls are one
	// pixel thick, so a ball allowed to move a full pixel per check could pass
	// clean through one. Anything below 1 prevents that; this leaves margin.
	substep = 0.4
	// grabRadius is how close the ball's center must come to a hole's center to
	// fall in. Slightly over a pixel, so a hole is caught by rolling over it
	// rather than by landing on it exactly.
	grabRadius = 1.2

	frame = 16 * time.Millisecond // ~60 fps
)

// Difficulty. Each level carves a fresh maze with more holes in it, up to a cap
// past which a maze runs out of dead ends to put them in anyway.
const (
	baseHoles = 3
	holesPer  = 2 // added per level
	maxHoles  = 14
)

var (
	wall  = color.RGBA{34, 44, 92, 255} // tunnel: a dim, cold blue
	ball  = color.RGBA{255, 255, 255, 255}
	goal  = color.RGBA{0, 147, 60, 255}   // MTA green: the way out
	hole  = color.RGBA{140, 16, 14, 255}  // MTA red, banked down: the way to lose
	drain = color.RGBA{255, 60, 50, 255}  // the flash when the ball goes down one
	text  = color.RGBA{212, 175, 55, 255} // gold, as on the clock
)

// outcome is what a frame of physics did.
type outcome int

const (
	rolling outcome = iota // still in play
	fell                   // down a hole: the level restarts
	reached                // reached the goal: on to the next maze
)

// game is one attempt at one maze: the maze, and the ball's position and
// velocity in pixel coordinates. The ball is tracked as a point, not a disc —
// at this resolution it is one pixel, and a point against a pixel wall grid is
// both simpler and exactly as accurate.
type game struct {
	m      *maze
	x, y   float32
	vx, vy float32
	pulse  int // frame counter, for the goal's breathing
}

// newGame puts the ball at rest in the maze's start cell.
func newGame(m *maze) *game {
	x, y := m.center(m.start)
	return &game{m: m, x: float32(x), y: float32(y)}
}

// step advances the physics by one frame under the given lean, and reports what
// became of the ball.
func (g *game) step(t input.Tilt) outcome {
	g.pulse++

	// The lean is an acceleration, not a velocity: it's what makes the ball
	// carry momentum through a turn instead of stopping dead when the player
	// levels the board.
	g.vx = (g.vx + t.X*gravity) * drag
	g.vy = (g.vy + t.Y*gravity) * drag

	// Walk the frame's motion in pieces small enough that the ball can't skip
	// over a one-pixel wall between checks — and check the holes each piece too,
	// so a fast ball falls in one instead of flying over it.
	steps := 1 + int(maxf(absf(g.vx), absf(g.vy))/substep)
	for i := 0; i < steps; i++ {
		g.advance(float32(steps))
		if g.overHole() {
			return fell
		}
	}

	if g.cell() == g.m.goal {
		return reached
	}
	return rolling
}

// advance moves the ball one substep, resolving each axis against the walls on
// its own. Axis-at-a-time is what lets the ball slide along a wall it's pressed
// against instead of sticking to it: the blocked axis stops, the free one keeps
// going.
func (g *game) advance(steps float32) {
	if nx := g.x + g.vx/steps; g.m.wallAt(round(nx), round(g.y)) {
		g.vx = -g.vx * restitution
	} else {
		g.x = nx
	}
	if ny := g.y + g.vy/steps; g.m.wallAt(round(g.x), round(ny)) {
		g.vy = -g.vy * restitution
	} else {
		g.y = ny
	}
}

// overHole reports whether the ball's center has come within grabRadius of a
// hole's mouth.
func (g *game) overHole() bool {
	for _, c := range g.m.holes {
		hx, hy := g.m.center(c)
		dx, dy := g.x-float32(hx), g.y-float32(hy)
		if dx*dx+dy*dy <= grabRadius*grabRadius {
			return true
		}
	}
	return false
}

// cell is the maze cell the ball is currently in.
func (g *game) cell() cell {
	return cell{
		x: (round(g.x) - g.m.offX) / pitch,
		y: (round(g.y) - g.m.offY) / pitch,
	}
}

// reset puts the ball back at the start, at rest, on the same maze.
func (g *game) reset() {
	x, y := g.m.center(g.m.start)
	g.x, g.y = float32(x), float32(y)
	g.vx, g.vy = 0, 0
}

// render draws the maze, its holes, the goal, and the ball.
func (g *game) render(d Display) {
	d.Clear()
	for y := 0; y < g.m.h; y++ {
		for x := 0; x < g.m.w; x++ {
			if g.m.wallAt(x, y) {
				d.SetPixel(int16(x), int16(y), wall)
			}
		}
	}
	for _, c := range g.m.holes {
		x, y := g.m.center(c)
		d.SetPixel(int16(x), int16(y), hole)
	}
	gx, gy := g.m.center(g.m.goal)
	d.SetPixel(int16(gx), int16(gy), breathe(goal, g.pulse))
	d.SetPixel(int16(round(g.x)), int16(round(g.y)), ball)
	d.Display()
}

// breathe dims c on a slow sine, so the goal pulses and reads as the thing to
// head for rather than as another piece of scenery.
func breathe(c color.RGBA, frame int) color.RGBA {
	// 0.55..1.0 over a ~1.6s cycle.
	k := 0.775 + 0.225*math.Sin(float64(frame)*0.065)
	return color.RGBA{uint8(float64(c.R) * k), uint8(float64(c.G) * k), uint8(float64(c.B) * k), 255}
}

// flashDrain blinks the ball's last position red: the ball went down a hole, and
// it should be obvious where.
func (g *game) flashDrain(d Display) {
	x, y := int16(round(g.x)), int16(round(g.y))
	for blink := 0; blink < 6; blink++ {
		c := drain
		if blink%2 == 1 {
			c = hole
		}
		d.SetPixel(x, y, c)
		d.Display()
		time.Sleep(90 * time.Millisecond)
	}
}

// flashWin lights the whole maze green: the ball is out.
func (g *game) flashWin(d Display) {
	for blink := 0; blink < 4; blink++ {
		c := wall
		if blink%2 == 0 {
			c = goal
		}
		for y := 0; y < g.m.h; y++ {
			for x := 0; x < g.m.w; x++ {
				if g.m.wallAt(x, y) {
					d.SetPixel(int16(x), int16(y), c)
				}
			}
		}
		d.Display()
		time.Sleep(110 * time.Millisecond)
	}
}

// drawLevel announces the level about to start.
func drawLevel(d Display, level int) {
	w, h := d.Size()
	d.Clear()
	s := "LEVEL " + strconv.Itoa(level)
	_, tw := tinyfont.LineWidth(&tinyfont.Picopixel, s)
	tinyfont.WriteLine(d, &tinyfont.Picopixel, (w-int16(tw))/2, h/2+2, s, text)
	d.Display()
	time.Sleep(1200 * time.Millisecond)
}

// holesFor is how many holes level gets, capped.
func holesFor(level int) int {
	n := baseHoles + (level-1)*holesPer
	if n > maxHoles {
		return maxHoles
	}
	return n
}

// Run plays the tilt maze forever. Each level is a fresh maze; falling down a
// hole restarts the level, and reaching the goal moves on to the next one.
func Run(cfg config.Config, d Display, in input.TiltSource) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for level := 1; ; level++ {
		drawLevel(d, level)
		g := newGame(newMaze(cfg.Width, cfg.Height, holesFor(level), rng))

		for out := rolling; out != reached; {
			g.render(d)
			time.Sleep(frame)

			out = g.step(in.Tilt())
			if out == fell {
				g.flashDrain(d)
				g.reset()
			}
		}
		g.flashWin(d)
	}
}

func absf(f float32) float32 {
	if f < 0 {
		return -f
	}
	return f
}

func maxf(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

// round snaps a ball coordinate to the pixel it sits on.
func round(f float32) int { return int(math.Round(float64(f))) }
