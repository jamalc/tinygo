// Package breakout runs Breakout on a HUB75-style display. Lean the board left
// or right to slide the paddle; knock out the wall of bricks without dropping
// the ball.
//
// It reads the same analog input.TiltSource the tilt maze does, but uses only
// its X axis — a paddle has one degree of freedom, and pretending otherwise
// would be dishonest. The lean drives the paddle's *acceleration* rather than
// its position: a position mapping would work from an accelerometer but would
// snap the paddle to the extremes from a keyboard, where a held key is all or
// nothing. Accelerating gives the paddle a little weight and reads the same way
// from either.
//
// It also reads an input.Source, for one thing only: launching the serve. The
// ball waits on the paddle, blinking, until any button fires — or until the
// grace period ends and it serves itself, so a board with nothing to press
// still gets a game.
//
// Like the other apps it drives any Display, so the same code runs on the panel
// and in the host simulator.
package breakout

import (
	"image/color"
	"math"
	"strconv"
	"time"

	"tinygo.org/x/tinyfont"

	"github.com/jamalc/tinygo/internal/config"
	"github.com/jamalc/tinygo/internal/input"
)

// Display is the subset of a panel the game drives. Both hub75.Device and
// simulator.Display satisfy it. Size is here because tinyfont needs it to write
// the level and score text.
type Display interface {
	SetPixel(x, y int16, c color.RGBA)
	Size() (x, y int16)
	Display() error
	Clear()
}

// The brick wall. Bricks are laid on a pitch and drawn a pixel narrower and
// shorter than it, which is what puts the mortar line between them — there is no
// separate gap to track.
const (
	brickPitchX = 8 // 8 bricks across a 64px panel
	brickPitchY = 3
	brickW      = 7 // drawn width: the 8th pixel is the mortar
	brickH      = 2
	brickTop    = 3 // leaves a row of clear air above the wall
	brickRows   = 4
)

// The paddle and the ball.
const (
	paddleW = 9
	// paddleLift is how far above the bottom the paddle sits. The row below it
	// is where the ball is lost, and where the lives are drawn.
	paddleLift = 2

	// paddleAccel is the paddle's acceleration at full lean, in px/frame².
	paddleAccel = 0.34
	// paddleDrag bleeds the paddle's speed each frame, so it coasts to a stop
	// when the board is levelled instead of sliding forever.
	paddleDrag = 0.80
	// paddleMax caps the paddle's speed. Fast enough to cross the panel in
	// about a second, slow enough that a save takes planning.
	paddleMax = 1.7

	// substep caps how far the ball moves between collision checks. Bricks and
	// the paddle are one and two pixels thick, so a ball allowed to move a whole
	// pixel per check could pass clean through one.
	substep = 0.4
	// steer is how much of the ball's speed the paddle's edges can redirect
	// sideways. Capped below 1 so a bounce always keeps some upward motion and
	// the ball can never end up travelling flat along the paddle forever.
	steer = 0.85

	frame = 16 * time.Millisecond // ~60 fps
)

// Difficulty. The ball speeds up each level; the paddle does not.
const (
	startLives = 3
	baseSpeed  = 0.55 // px/frame
	speedPer   = 0.08 // added per level
	maxSpeed   = 1.15
)

// The serve. A served ball waits on the paddle for the player's press.
const (
	// serveGrace is how long a waiting ball serves itself after, in frames —
	// three seconds at 60 fps. It is what keeps the game playable on an input
	// with no button at all (a bare accelerometer board with none mapped), and
	// what preserves the old attract-mode feel for a player who only watches.
	serveGrace = 180

	// serveBlink is the half-period, in frames, of the waiting ball's blink —
	// the whole of the "press to serve" cue.
	serveBlink = 10
)

// brickColors paints each row of the wall in an MTA line color, so the wall
// reads as a stack of subway lines. Same values the clock draws its route
// bullets with (internal/clock/colors.go), duplicated rather than shared: they
// mean "a subway line's colour" there and "a brick's paint" here.
var brickColors = []color.RGBA{
	{238, 53, 46, 255},  // 1 2 3
	{252, 204, 10, 255}, // N Q R W
	{0, 147, 60, 255},   // 4 5 6
	{0, 57, 166, 255},   // A C E
}

var (
	ballColor   = color.RGBA{255, 255, 255, 255}
	paddleColor = color.RGBA{167, 169, 172, 255} // MTA grey
	lifeColor   = color.RGBA{0, 147, 60, 255}    // green: a life still in hand
	lostColor   = color.RGBA{238, 53, 46, 255}   // red: the flash when one goes
	textColor   = color.RGBA{212, 175, 55, 255}  // gold, as on the clock
)

// outcome is what a frame did.
type outcome int

const (
	playing outcome = iota // still in play
	lost                   // the ball went past the paddle
	cleared                // the last brick is gone
)

// game is one game: the wall, the paddle, the ball, and the score. Positions are
// pixel coordinates; the paddle's x is its left edge, and the ball is a point.
type game struct {
	w, h   int
	cols   int
	bricks []bool // row-major, cols wide; true means still standing

	paddleX float32
	paddleV float32

	x, y   float32
	vx, vy float32

	// serving is true while the ball waits on the paddle for its launch;
	// serveWait counts the frames it has waited, toward serveGrace.
	serving   bool
	serveWait int

	lives int
	score int
	level int
}

// newGame racks up a fresh game: a full wall, three lives, and a served ball.
func newGame(w, h int) *game {
	g := &game{
		w:     w,
		h:     h,
		cols:  w / brickPitchX,
		lives: startLives,
		level: 1,
	}
	g.bricks = make([]bool, g.cols*brickRows)
	g.rack()
	return g
}

// rack restores the wall and re-centers the paddle, then serves.
func (g *game) rack() {
	for i := range g.bricks {
		g.bricks[i] = true
	}
	g.paddleX = float32(g.w-paddleW) / 2
	g.paddleV = 0
	g.serve()
}

// serve parks the ball on the paddle to wait for its launch: the player's
// press, or serveGrace running out.
func (g *game) serve() {
	g.serving = true
	g.serveWait = 0
	g.vx, g.vy = 0, 0
	g.followPaddle()
}

// followPaddle sits the waiting ball on the paddle's center, so it rides along
// as the player lines the serve up.
func (g *game) followPaddle() {
	g.x = g.paddleX + (paddleW-1)/2
	g.y = float32(g.paddleRow() - 1)
}

// launch sends the waiting ball up and to the right.
func (g *game) launch() {
	g.serving = false

	// Off the vertical, so the first bounce doesn't just come straight back.
	speed := g.speed()
	g.vx = speed * 0.45
	g.vy = -speed * float32(math.Sqrt(1-0.45*0.45))
}

// speed is the ball's speed at the current level, capped.
func (g *game) speed() float32 {
	s := baseSpeed + float32(g.level-1)*speedPer
	if s > maxSpeed {
		return maxSpeed
	}
	return s
}

// paddleRow is the y of the paddle's single-pixel row.
func (g *game) paddleRow() int { return g.h - 1 - paddleLift }

// step advances one frame under the given lean and reports what became of it.
// fire is the player's press, if one arrived this frame; it launches a waiting
// serve and means nothing otherwise.
func (g *game) step(t input.Tilt, fire bool) outcome {
	g.movePaddle(t)

	if g.serving {
		g.followPaddle()
		g.serveWait++
		if !fire && g.serveWait < serveGrace {
			return playing
		}
		g.launch() // and fall through: the ball moves on the frame it's served
	}

	// Walk the ball's motion in pieces small enough that it can't skip over a
	// brick or the paddle between checks.
	steps := 1 + int(maxf(absf(g.vx), absf(g.vy))/substep)
	for i := 0; i < steps; i++ {
		g.advance(float32(steps))
		if g.y > float32(g.h-1) {
			return lost
		}
	}

	if g.standing() == 0 {
		return cleared
	}
	return playing
}

// movePaddle accelerates the paddle along the lean, then slides it, stopping at
// the walls. Only the lean's X matters: the paddle has one axis.
func (g *game) movePaddle(t input.Tilt) {
	g.paddleV = (g.paddleV + t.X*paddleAccel) * paddleDrag
	g.paddleV = clampf(g.paddleV, -paddleMax, paddleMax)

	g.paddleX = clampf(g.paddleX+g.paddleV, 0, float32(g.w-paddleW))
	// A paddle jammed against a wall has no speed left; without this it would
	// hold its stored velocity and leap away the instant the lean eased off.
	if g.paddleX <= 0 || g.paddleX >= float32(g.w-paddleW) {
		g.paddleV = 0
	}
}

// advance moves the ball one substep, resolving each axis against the bricks and
// the walls on its own, then against the paddle. Axis-at-a-time is what gives a
// brick its corners: a ball entering from the side reverses only its horizontal
// motion, and one entering from below only its vertical.
func (g *game) advance(steps float32) {
	if nx := g.x + g.vx/steps; g.strike(nx, g.y) {
		g.vx = -g.vx
	} else {
		g.x = nx
	}
	if ny := g.y + g.vy/steps; g.strike(g.x, ny) {
		g.vy = -g.vy
	} else {
		g.y = ny
	}
	g.bounceOffPaddle()
}

// strike reports whether the ball is blocked at the given point, knocking the
// brick out if a brick is what's there. The side walls and the ceiling block;
// the floor does not, because falling through it is how a life is lost.
func (g *game) strike(x, y float32) bool {
	px, py := round(x), round(y)
	if px < 0 || px >= g.w || py < 0 {
		return true
	}
	if i := g.brickAt(px, py); i >= 0 {
		g.bricks[i] = false
		g.score++
		return true
	}
	return false
}

// bounceOffPaddle reflects a descending ball off the paddle, steering it by
// where along the paddle it landed: the middle sends it back up, and the ends
// throw it out to the side. That is the whole skill of the game — the paddle
// aims the ball, it doesn't merely return it.
func (g *game) bounceOffPaddle() {
	if g.vy <= 0 || round(g.y) < g.paddleRow() {
		return
	}
	if g.x < g.paddleX-0.5 || g.x > g.paddleX+paddleW-0.5 {
		return // past the end of the paddle: not a save
	}

	// -1 at the left tip, +1 at the right.
	center := g.paddleX + (paddleW-1)/2
	off := clampf((g.x-center)/((paddleW-1)/2), -1, 1) * steer

	speed := g.speed()
	g.vx = speed * off
	g.vy = -speed * float32(math.Sqrt(float64(1-off*off)))
	g.y = float32(g.paddleRow() - 1) // sit the ball on top, never inside
}

// brickAt returns the index of the brick occupying the pixel, or -1. The mortar
// pixels between bricks belong to no brick, so the ball passes through them and
// strikes the brick behind.
func (g *game) brickAt(px, py int) int {
	if py < brickTop || py >= brickTop+brickRows*brickPitchY {
		return -1
	}
	row, into := (py-brickTop)/brickPitchY, (py-brickTop)%brickPitchY
	if into >= brickH {
		return -1 // the mortar line under a row
	}
	col := px / brickPitchX
	if col >= g.cols || px%brickPitchX >= brickW {
		return -1 // past the wall, or the mortar line beside a brick
	}
	if i := row*g.cols + col; g.bricks[i] {
		return i
	}
	return -1
}

// standing counts the bricks still up.
func (g *game) standing() int {
	n := 0
	for _, up := range g.bricks {
		if up {
			n++
		}
	}
	return n
}

// render draws the wall, the paddle, the ball, and the lives still in hand.
func (g *game) render(d Display) {
	d.Clear()

	for row := 0; row < brickRows; row++ {
		for col := 0; col < g.cols; col++ {
			if !g.bricks[row*g.cols+col] {
				continue
			}
			x0, y0 := col*brickPitchX, brickTop+row*brickPitchY
			for y := y0; y < y0+brickH; y++ {
				for x := x0; x < x0+brickW; x++ {
					d.SetPixel(int16(x), int16(y), brickColors[row%len(brickColors)])
				}
			}
		}
	}

	for i := 0; i < paddleW; i++ {
		d.SetPixel(int16(round(g.paddleX)+i), int16(g.paddleRow()), paddleColor)
	}
	// A waiting ball blinks — the whole of the "press to serve" cue.
	if !g.serving || (g.serveWait/serveBlink)%2 == 0 {
		d.SetPixel(int16(round(g.x)), int16(round(g.y)), ballColor)
	}

	// The lives in hand, as dots in the bottom-right corner. The one being
	// played isn't shown — these are the spares.
	for i := 0; i < g.lives-1; i++ {
		d.SetPixel(int16(g.w-1-i*2), int16(g.h-1), lifeColor)
	}

	d.Display()
}

// flashLoss blinks the paddle red: the ball got past it.
func (g *game) flashLoss(d Display) {
	for blink := 0; blink < 6; blink++ {
		c := lostColor
		if blink%2 == 1 {
			c = paddleColor
		}
		for i := 0; i < paddleW; i++ {
			d.SetPixel(int16(round(g.paddleX)+i), int16(g.paddleRow()), c)
		}
		d.Display()
		time.Sleep(90 * time.Millisecond)
	}
}

// banner clears the panel and centers one line on it.
func banner(d Display, s string, c color.RGBA, hold time.Duration) {
	w, h := d.Size()
	d.Clear()
	_, tw := tinyfont.LineWidth(&tinyfont.Picopixel, s)
	tinyfont.WriteLine(d, &tinyfont.Picopixel, (w-int16(tw))/2, h/2+2, s, c)
	d.Display()
	time.Sleep(hold)
}

// Run plays Breakout forever: clear the wall to move up a level, drop the ball
// three times and the game starts over.
//
// Each serve waits on the paddle for a press — any Button src reports, because
// every discrete control this hardware has means "go" when the game is the only
// thing listening — and serves itself after serveGrace, so a src that never
// fires (a disabled button Source) still gets a game.
func Run(cfg config.Config, d Display, in input.TiltSource, src input.Source) {
	g := newGame(cfg.Width, cfg.Height)
	banner(d, "LEVEL 1", textColor, 1200*time.Millisecond)

	for {
		g.render(d)
		time.Sleep(frame)

		switch g.step(in.Tilt(), src.Poll() != input.None) {
		case lost:
			g.flashLoss(d)
			g.lives--
			if g.lives > 0 {
				g.serve()
				continue
			}
			banner(d, "GAME OVER", ballColor, 1200*time.Millisecond)
			banner(d, strconv.Itoa(g.score), textColor, 1800*time.Millisecond)
			g = newGame(cfg.Width, cfg.Height)
			banner(d, "LEVEL 1", textColor, 1200*time.Millisecond)

		case cleared:
			g.level++
			g.rack()
			banner(d, "LEVEL "+strconv.Itoa(g.level), textColor, 1200*time.Millisecond)
		}
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

func clampf(f, lo, hi float32) float32 {
	switch {
	case f < lo:
		return lo
	case f > hi:
		return hi
	}
	return f
}

// round snaps a ball or paddle coordinate to the pixel it sits on.
func round(f float32) int { return int(math.Round(float64(f))) }
