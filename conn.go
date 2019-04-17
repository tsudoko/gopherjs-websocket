// Copyright 2014-2015 GopherJS Team. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be found
// in the LICENSE file.

package websocket

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"

	"github.com/gopherjs/gopherjs/js"
	"github.com/gopherjs/websocket/websocketjs"
)

const (
	TextMessage   = 1
	BinaryMessage = 2
	CloseMessage  = 8
	PingMessage   = 9
	PongMessage   = 10
)

func beginHandlerOpen(ch chan error, removeHandlers func()) func(ev *js.Object) {
	return func(ev *js.Object) {
		removeHandlers()
		close(ch)
	}
}

// closeError allows a CloseEvent to be used as an error.
type closeError struct {
	*js.Object
	Code     int    `js:"code"`
	Reason   string `js:"reason"`
	WasClean bool   `js:"wasClean"`
}

func (e *closeError) Error() string {
	var cleanStmt string
	if e.WasClean {
		cleanStmt = "clean"
	} else {
		cleanStmt = "unclean"
	}
	return fmt.Sprintf("CloseEvent: (%s) (%d) %s", cleanStmt, e.Code, e.Reason)
}

func beginHandlerClose(ch chan error, removeHandlers func()) func(ev *js.Object) {
	return func(ev *js.Object) {
		removeHandlers()
		go func() {
			ch <- &closeError{Object: ev}
			close(ch)
		}()
	}
}

type deadlineErr struct{}

func (e *deadlineErr) Error() string   { return "i/o timeout: deadline reached" }
func (e *deadlineErr) Timeout() bool   { return true }
func (e *deadlineErr) Temporary() bool { return true }

var errDeadlineReached = &deadlineErr{}

// TODO(nightexcessive): Add a Dial function that allows a deadline to be
// specified.

// Dial opens a new WebSocket connection. It will block until the connection is
// established or fails to connect.
func Dial(url string) (*Conn, error) {
	ws, err := websocketjs.New(url)
	if err != nil {
		return nil, err
	}
	conn := &Conn{
		WebSocket: ws,
		ch:        make(chan *messageEvent, 1),
	}
	conn.initialize()

	openCh := make(chan error, 1)

	var (
		openHandler  func(ev *js.Object)
		closeHandler func(ev *js.Object)
	)

	// Handlers need to be removed to prevent a panic when the WebSocket closes
	// immediately and fires both open and close before they can be removed.
	// This way, handlers are removed before the channel is closed.
	removeHandlers := func() {
		ws.RemoveEventListener("open", false, openHandler)
		ws.RemoveEventListener("close", false, closeHandler)
	}

	// We have to use variables for the functions so that we can remove the
	// event handlers afterwards.
	openHandler = beginHandlerOpen(openCh, removeHandlers)
	closeHandler = beginHandlerClose(openCh, removeHandlers)

	ws.AddEventListener("open", false, openHandler)
	ws.AddEventListener("close", false, closeHandler)

	err, ok := <-openCh
	if ok && err != nil {
		return nil, err
	}

	return conn, nil
}

// Conn is a high-level wrapper around WebSocket.
type Conn struct {
	*websocketjs.WebSocket

	ch      chan *messageEvent
	readBuf *bytes.Reader

	readDeadline time.Time
}

type messageEvent struct {
	*js.Object
	Data *js.Object `js:"data"`
}

func (c *Conn) onMessage(event *js.Object) {
	go func() {
		c.ch <- &messageEvent{Object: event}
	}()
}

func (c *Conn) onClose(event *js.Object) {
	go func() {
		// We queue nil to the end so that any messages received prior to
		// closing get handled first.
		c.ch <- nil
	}()
}

// initialize adds all of the event handlers necessary for a Conn to function.
// It should never be called more than once and is already called if Dial was
// used to create the Conn.
func (c *Conn) initialize() {
	// We need this so that received binary data is in ArrayBufferView format so
	// that it can easily be read.
	c.BinaryType = "arraybuffer"

	c.AddEventListener("message", false, c.onMessage)
	c.AddEventListener("close", false, c.onClose)
}

// handleFrame handles a single frame received from the channel. This is a
// convenience funciton to dedupe code for the multiple deadline cases.
func (c *Conn) handleFrame(message *messageEvent, ok bool) (*messageEvent, error) {
	if !ok { // The channel has been closed
		return nil, io.EOF
	} else if message == nil {
		// See onClose for the explanation about sending a nil item.
		close(c.ch)
		return nil, io.EOF
	}

	return message, nil
}

// receiveFrame receives one full frame from the WebSocket. It blocks until the
// frame is received.
func (c *Conn) receiveFrame(observeDeadline bool) (*messageEvent, error) {
	var deadlineChan <-chan time.Time // Receiving on a nil channel always blocks indefinitely

	if observeDeadline && !c.readDeadline.IsZero() {
		now := time.Now()
		if now.After(c.readDeadline) {
			select {
			case item, ok := <-c.ch:
				return c.handleFrame(item, ok)
			default:
				return nil, errDeadlineReached
			}
		}

		timer := time.NewTimer(c.readDeadline.Sub(now))
		defer timer.Stop()

		deadlineChan = timer.C
	}

	select {
	case item, ok := <-c.ch:
		return c.handleFrame(item, ok)
	case <-deadlineChan:
		return nil, errDeadlineReached
	}
}

func getFrameData(obj *js.Object) []byte {
	// Check if it's an array buffer. If so, convert it to a Go byte slice.
	if constructor := obj.Get("constructor"); constructor == js.Global.Get("ArrayBuffer") {
		uint8Array := js.Global.Get("Uint8Array").New(obj)
		return uint8Array.Interface().([]byte)
	}
	return []byte(obj.String())
}

func (c *Conn) ReadMessage() (_ int, p []byte, err error) {
	frame, err := c.receiveFrame(true)
	if err != nil {
		return 0, nil, err
	}

	receivedBytes := getFrameData(frame.Data)
	return BinaryMessage, receivedBytes, nil
}

// WriteMessage writes the contents of p to the WebSocket using a binary opcode.
func (c *Conn) WriteMessage(_ int, p []byte) (err error) {
	// []byte is converted to an Uint8Array by GopherJS, which fullfils the
	// ArrayBufferView definition.
	return c.Send(p)
}

// LocalAddr would typically return the local network address, but due to
// limitations in the JavaScript API, it is unable to. Calling this method will
// cause a panic.
func (c *Conn) LocalAddr() net.Addr {
	// BUG(nightexcessive): Conn.LocalAddr() panics because the underlying
	// JavaScript API has no way of figuring out the local address.

	// TODO(nightexcessive): Find a more graceful way to handle this
	panic("we are unable to implement websocket.Conn.LocalAddr() due to limitations in the underlying JavaScript API")
}

// RemoteAddr returns the remote network address, based on
// websocket.WebSocket.URL.
func (c *Conn) RemoteAddr() net.Addr {
	wsURL, err := url.Parse(c.URL)
	if err != nil {
		// TODO(nightexcessive): Should we be panicking for this?
		panic(err)
	}
	return &addr{wsURL}
}

// SetDeadline sets the read and write deadlines associated with the connection.
// It is equivalent to calling both SetReadDeadline and SetWriteDeadline.
//
// A zero value for t means that I/O operations will not time out.
func (c *Conn) SetDeadline(t time.Time) error {
	c.readDeadline = t
	return nil
}

// SetReadDeadline sets the deadline for future Read calls. A zero value for t
// means Read will not time out.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.readDeadline = t
	return nil
}

// SetWriteDeadline sets the deadline for future Write calls. Because our writes
// do not block, this function is a no-op.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return nil
}
