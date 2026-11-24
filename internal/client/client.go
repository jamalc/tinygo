// Package client fetches arrival data, and Citi Bike stations, from the serve
// backend over HTTP.
//
// Used by the clock and docks loops on the simulator and on every device. There is one transport:
// net/http, over whatever Netdever the board registers — netlink on the Matrix Portal M4,
// espradio on the MatrixPortal S3, internal/wifi's lneto bridge on the Interstate 75 W.
// Nothing here is board-specific.
package client

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jamalc/tinygo/internal/api"
	"github.com/jamalc/tinygo/internal/config"
)

// Client fetches arrivals for a fixed set of stops. The request URL is built once at
// construction (the stops don't change), so polling reuses it.
type Client struct {
	url   string
	board bool // the response is an api.Board (format=board), not a bare group list
}

// NewClient builds a Client that polls cfg.Host:cfg.Port for cfg.Stops. It requests
// format=flat, so the server returns the groups already flattened and sorted (stations
// in cfg.Stops order, then route, then direction) and the device renders them as-is.
// With cfg.Bikes set it asks for format=board instead: the same groups, plus those
// Citi Bike stations, in one response. (format=board needs a server new enough to
// know it; with no bikes the request stays format=flat, which any server answers.)
func NewClient(cfg config.Config) *Client {
	var u url.URL
	u.Scheme = "http"
	u.Host = cfg.Host + ":" + strconv.Itoa(cfg.Port)
	u.Path = "/arrivals"
	q := u.Query()
	for _, stop := range cfg.Stops {
		q.Add("stop", stop)
	}
	// LIRR trips come back as one more group each, after the stops' (see
	// cfg.LIRR); both formats carry them.
	for _, trip := range cfg.LIRR {
		q.Add("lirr", trip)
	}
	board := len(cfg.Bikes) > 0
	if board {
		q.Set("format", "board")
		for _, b := range cfg.Bikes {
			q.Add("bike", b)
		}
	} else {
		q.Set("format", "flat")
	}
	// The server takes the horizon in minutes. A config horizon under a minute
	// (including an unset or unparseable one) leaves the param off entirely, so
	// the server falls back to its own default rather than to zero.
	if mins := int(cfg.Horizon / time.Minute); mins > 0 {
		q.Set("horizon", strconv.Itoa(mins))
	}
	u.RawQuery = q.Encode()
	return &Client{url: u.String(), board: board}
}

// LoadArrivals fetches the arrivals (and any bike stations) and decodes them into dst, and returns the server's clock at
// the time of the response (from its Date header; zero if absent or unparseable) — the device has
// no real-time clock, so this is how it learns the wall time. It reads the response into *buf
// and decodes dst's backing in place — both reused across calls — so a steady poll loop makes NO
// large per-fetch allocation. That matters on the RAM-tight ESP32-S3: on TinyGo's non-compacting heap,
// fresh per-poll buffers break up the contiguous blocks a later fetch needs, even when total bytes
// stay flat (see the repo README, "Memory on TinyGo"). *buf grows only when a response outgrows it and
// is reused otherwise; dst's slice backing is reused by Unmarshal. The caller owns *buf and must
// not share it across concurrent fetches. On a transport or status error dst is left untouched; a
// decode error (a malformed response) may leave it partially updated, which self-heals next poll.
func (c *Client) LoadArrivals(dst *api.Board, buf *[]byte) (time.Time, error) {
	t := startTrace()

	body, date, err := openBody(c.url)
	if err != nil {
		return time.Time{}, err
	}
	defer body.Close()
	t.mark("body")

	n, err := readInto(body, buf)
	if err != nil {
		return time.Time{}, err
	}
	t.mark("read")

	// Bikes is emptied first, keeping its backing: a flat response has none, and
	// this way a board response that somehow left the field out can't leave the
	// previous poll's stations in place either.
	dst.Bikes = dst.Bikes[:0]
	if c.board {
		err = json.Unmarshal((*buf)[:n], dst)
	} else {
		err = json.Unmarshal((*buf)[:n], &dst.Groups)
	}
	t.mark("decode")
	t.report("poll")
	return date, err
}

// readInto reads r fully into *buf, returning the byte count. It grows *buf (doubling, min 4 KB)
// only when a response outgrows the current capacity and reuses it otherwise — so after the first
// fetch a steady poll loop reads into the same buffer with no fresh allocation, which is what keeps
// the ESP32-S3 heap from fragmenting.
func readInto(r io.Reader, buf *[]byte) (int, error) {
	b := *buf
	n := 0
	for {
		if n == len(b) {
			grow := 2 * len(b)
			if grow < 4096 {
				grow = 4096
			}
			nb := make([]byte, grow)
			copy(nb, b[:n])
			b = nb
			*buf = b
		}
		m, err := r.Read(b[n:])
		n += m
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// httpClient is built per platform: with a timeout on the host, without one under
// TinyGo, whose net/http panics on any failed request when a timeout is set. See
// http_host.go and http_tinygo.go.

// BikeClient fetches a fixed set of Citi Bike stations from the server's /bikes, for the docks
// app. Like Client, it builds its URL once and reads into caller-owned buffers.
type BikeClient struct {
	url string
}

// NewBikeClient builds a BikeClient that polls cfg.Host:cfg.Port for the stations in cfg.Bikes.
func NewBikeClient(cfg config.Config) *BikeClient {
	var u url.URL
	u.Scheme = "http"
	u.Host = cfg.Host + ":" + strconv.Itoa(cfg.Port)
	u.Path = "/bikes"
	q := u.Query()
	for _, b := range cfg.Bikes {
		q.Add("station", b)
	}
	u.RawQuery = q.Encode()
	return &BikeClient{url: u.String()}
}

// LoadStations fetches the stations, in config order, into *dst, and returns the server's clock
// at the time of the response (zero if it didn't say). It reuses *buf and *dst's backing across
// calls exactly as Client.LoadArrivals does, for the same reason; the same ownership rules apply.
// *dst is emptied and zeroed before decoding, so a response can never leave a previous poll's
// stations, or any of their fields, in it.
func (c *BikeClient) LoadStations(dst *[]api.BikeStation, buf *[]byte) (time.Time, error) {
	t := startTrace()

	body, date, err := openBody(c.url)
	if err != nil {
		return time.Time{}, err
	}
	defer body.Close()
	t.mark("body")

	n, err := readInto(body, buf)
	if err != nil {
		return time.Time{}, err
	}
	t.mark("read")

	// Unmarshal reuses the backing's elements without zeroing them, so a field a response left
	// out would keep the previous poll's value. Clearing the whole backing first rules that out.
	clear((*dst)[:cap(*dst)])
	*dst = (*dst)[:0]
	err = json.Unmarshal((*buf)[:n], dst)
	t.mark("decode")
	t.report("bikes")
	return date, err
}

// openBody issues the GET and hands back the response body as a stream for the caller to
// read into its caller-owned buffer (see readInto), along with the response's Date. Returning the stream rather than the
// bytes is what lets that buffer be reused across polls, which on a non-compacting heap is
// the difference between a steady poll loop and one that fragments into an OOM.
func openBody(url string) (io.ReadCloser, time.Time, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, time.Time{}, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, time.Time{}, errors.New(resp.Status)
	}
	return resp.Body, responseDate(resp.Header.Get("Date")), nil
}

// responseDate parses an HTTP Date header ("Mon, 02 Jan 2006 15:04:05 GMT"), returning the zero
// time when it's missing or malformed so the caller can tell "no clock" from a real one.
func responseDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(http.TimeFormat, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
