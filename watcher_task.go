package wadjit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"github.com/jakobilobi/go-taskman"
	"github.com/rs/xid"
)

// WatcherTask is a task that the Watcher can execute to interact with a target endpoint.
type WatcherTask interface {
	// Close closes the WatcherTask, cleaning up and releasing resources.
	// Note: will block until the task is closed. (?)
	Close() error

	// Initialize sets up the WatcherTask to be ready to watch an endpoint.
	Initialize(id xid.ID, respChan chan<- WatcherResponse) error

	// Task returns a taskman.Task that sends requests and messages to the endpoint.
	Task() taskman.Task

	// Validate checks that the WatcherTask is ready for initialization.
	Validate() error
}

//
// HTTP
//

// HTTPEndpoint spawns tasks to make HTTP requests towards the defined endpoint. Implements the
// WatcherTask interface and is meant for use in a Watcher.
type HTTPEndpoint struct {
	URL     *url.URL
	Header  http.Header
	Payload []byte

	id       xid.ID
	respChan chan<- WatcherResponse
}

// Close closes the HTTP endpoint.
func (e *HTTPEndpoint) Close() error {
	return nil
}

// Initialize sets up the HTTP endpoint to be able to send on its responses.
func (e *HTTPEndpoint) Initialize(id xid.ID, responseChannel chan<- WatcherResponse) error {
	e.id = id
	e.respChan = responseChannel
	// TODO: set mode based on payload, e.g. JSON RPC, text ete.
	return nil
}

// Task returns a taskman.Task that sends an HTTP request to the endpoint.
func (e *HTTPEndpoint) Task() taskman.Task {
	return &httpRequest{
		endpoint: e,
		respChan: e.respChan,
		data:     e.Payload,
		method:   http.MethodGet,
	}
}

// Validate checks that the HTTPEndpoint is ready to be initialized.
func (e *HTTPEndpoint) Validate() error {
	if e.URL == nil {
		return errors.New("URL is nil")
	}
	if e.Header == nil {
		// Set empty header if nil
		e.Header = make(http.Header)
	}
	return nil
}

// httpRequest is an implementation of taskman.Task that sends an HTTP request to an endpoint.
type httpRequest struct {
	endpoint *HTTPEndpoint
	respChan chan<- WatcherResponse

	data   []byte
	method string
}

// Execute sends an HTTP request to the endpoint.
func (r httpRequest) Execute() error {
	request, err := http.NewRequest(r.method, r.endpoint.URL.String(), bytes.NewReader(r.data))
	if err != nil {
		r.respChan <- errorResponse(err, r.endpoint.id, r.endpoint.URL)
		return err
	}

	// Add tracing to the request
	timings := &httpRequestTimes{}
	trace := traceRequest(timings)
	ctx := httptrace.WithClientTrace(request.Context(), trace)
	request = request.WithContext(ctx)

	// Add headers to the request
	for key, values := range r.endpoint.Header {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}

	// Send the request
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		r.respChan <- errorResponse(err, r.endpoint.id, r.endpoint.URL)
		return err
	}

	// Create a task response
	taskResponse := NewHTTPTaskResponse(response)
	taskResponse.latency = timings.FirstResponseByte.Sub(timings.Start)
	taskResponse.receivedAt = time.Now()

	// Send the response on the channel
	r.respChan <- WatcherResponse{
		WatcherID: r.endpoint.id,
		URL:       r.endpoint.URL,
		Err:       nil,
		Payload:   taskResponse,
	}

	return nil
}

// httpRequestTimes stores the timestamps of an HTTP request used to calculate the latency.
type httpRequestTimes struct {
	Start             time.Time
	FirstResponseByte time.Time
}

func traceRequest(times *httpRequestTimes) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		// The earliest guaranteed callback is usually ConnectStart, so we set the start time there
		ConnectStart: func(_, _ string) {
			times.Start = time.Now()
		},
		GotFirstResponseByte: func() {
			times.FirstResponseByte = time.Now()
		},
	}
}

//
// WebSocket
//

// WSEndpoint connects to the target endpoint, and spawns tasks to send messages to that endpoint.
// Implements the WatcherTask interface and is meant for use in a Watcher.
type WSEndpoint struct {
	mu   sync.Mutex
	mode WSEndpointMode

	URL     *url.URL
	Header  http.Header
	Payload []byte

	conn         *websocket.Conn
	ctx          context.Context
	cancel       context.CancelFunc
	inflightMsgs sync.Map // Key string to value wsInflightMessage
	wg           sync.WaitGroup

	id       xid.ID
	respChan chan<- WatcherResponse
}

// WSEndpointMode is an enum for the mode of the WebSocket endpoint.
type WSEndpointMode int

const (
	ModeUnknown      WSEndpointMode = iota // Defaults to OneHitText
	OneHitText                             // One hit text mode is the default mode
	LongLivedJSONRPC                       // Long lived JSON RPC mode
)

// wsInflightMessage stores metadata about a message that is currently in-flight.
type wsInflightMessage struct {
	inflightID string
	originalID interface{}
	timeSent   time.Time
}

// Close closes the WebSocket connection, and cancels its context.
func (e *WSEndpoint) Close() error {
	e.lock()
	defer e.unlock()

	// If the connection is already closed, do nothing
	if e.conn == nil {
		return nil
	} else {
		// Send a close message
		formattedCloseMessage := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
		deadline := time.Now().Add(3 * time.Second)
		err := e.conn.WriteControl(websocket.CloseMessage, formattedCloseMessage, deadline)
		if err != nil {
			return err
		}
		// Close the connection
		err = e.conn.Close()
		if err != nil {
			return err
		}
		e.conn = nil
	}

	// Cancel the context
	e.cancel()

	return nil
}

// Initialize prepares the WSEndpoint to be able to send messages to the target endpoint.
// If configured as one of the persistent connection modes, e.g. JSON RPC, this function will
// establish a long-lived connection to the endpoint.
func (e *WSEndpoint) Initialize(id xid.ID, responseChannel chan<- WatcherResponse) error {
	e.mu.Lock()
	e.id = id
	e.respChan = responseChannel
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.mu.Unlock()

	switch e.mode {
	case LongLivedJSONRPC:
		err := e.connect()
		if err != nil {
			return fmt.Errorf("failed to connect when initializing: %w", err)
		}
	case OneHitText:
		// One hit modes do not require a connection to be established, so do nothing
	default:
		// Default to one hit text mode, since its a mode not requiring anything logic outside of its own scope
		e.mu.Lock()
		e.mode = OneHitText
		e.mu.Unlock()
	}

	return nil
}

// Task returns a taskman.Task that sends a message to the WebSocket endpoint.
func (e *WSEndpoint) Task() taskman.Task {
	switch e.mode {
	case OneHitText:
		return &wsOneHit{
			wsEndpoint: e,
		}
	case LongLivedJSONRPC:
		return &wsPersistent{
			protocol:   JSONRPC,
			wsEndpoint: e,
		}
	default:
		// Default to one hit mode since it should work for most implementations
		return &wsOneHit{
			wsEndpoint: e,
		}
	}
}

// Validate checks that the WSEndpoint is ready to be initialized.
func (e *WSEndpoint) Validate() error {
	if e.URL == nil {
		return errors.New("URL is nil")
	}
	if e.Header == nil {
		// Set empty header if nil
		e.Header = make(http.Header)
	}
	return nil
}

// connect establishes a connection to the WebSocket endpoint. If already connected,
// this function does nothing.
func (e *WSEndpoint) connect() error {
	if e.mode != LongLivedJSONRPC {
		return errors.New("cannot establish long lived connection for non-long mode")
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	// Only connect if the connection is not already established
	if e.conn != nil {
		return fmt.Errorf("connection already established")
	}

	// Establish the connection
	conn, _, err := websocket.DefaultDialer.Dial(e.URL.String(), e.Header)
	if err != nil {
		return err
	}
	e.conn = conn

	// Start the read pump for incoming messages
	e.wg.Add(1)
	go e.readPump(&e.wg)

	return nil
}

// reconnect closes the current connection and establishes a new one.
func (e *WSEndpoint) reconnect() error {
	if e.mode != LongLivedJSONRPC {
		return errors.New("can only reconnect for long-lived connections")
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	// Close the current connection, if it exists
	if e.conn != nil {
		if err := e.conn.Close(); err != nil {
			return fmt.Errorf("failed to close connection: %w", err)
		}
	}

	// Wait for the read pump to finish
	e.wg.Wait()
	e.conn = nil

	// Establish a new connection
	conn, _, err := websocket.DefaultDialer.Dial(e.URL.String(), e.Header)
	if err != nil {
		return fmt.Errorf("failed to dial when reconnecting: %w", err)
	}
	e.conn = conn

	// Restart the read pump for incoming messages
	e.wg.Add(1)
	go e.readPump(&e.wg)

	return nil
}

// lock and unlock provide exclusive access to the connection's mutex.
func (e *WSEndpoint) lock() {
	e.mu.Lock()
}

func (e *WSEndpoint) unlock() {
	e.mu.Unlock()
}

// read reads messages from the WebSocket connection.
// Note: the read pump has exclusive permission to read from the connection.
func (e *WSEndpoint) readPump(wg *sync.WaitGroup) {
	defer func() {
		wg.Done()
	}()

	for {
		select {
		case <-e.ctx.Done():
			// Endpoint shutting down
			return
		default:
			// Read message from connection
			_, p, err := e.conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseServiceRestart) {
					// This is an expected situation, handle gracefully
				} else if strings.Contains(err.Error(), "connection closed") {
					// This is not an unknown situation, handle gracefully
				} else {
					// This is unexpected
					// TODO: add custom handling here?
				}

				// If there was an error, close the connection
				return
			}
			timeRead := time.Now()

			if e.mode == LongLivedJSONRPC {
				// 1. Unmarshal p into a JSON-RPC response interface
				jsonRPCResp := &JSONRPCResponse{}
				err = jsonRPCResp.ParseFromBytes(p, len(p))
				if err != nil {
					// Send an error response
					e.respChan <- errorResponse(err, e.id, e.URL)
					return
				}

				// 2. Check the ID against the inflight messages map
				if !jsonRPCResp.IsEmpty() {
					responseID := jsonRPCResp.ID()
					if responseID == nil {
						// Send an error response
						e.respChan <- errorResponse(err, e.id, e.URL)
						return
					}
					responseIDStr := fmt.Sprintf("%v", responseID)

					// 3. If the ID is known, get the inflight map metadata and delete the ID in the map
					if inflightMsg, ok := e.inflightMsgs.Load(responseIDStr); ok {
						inflightMsg := inflightMsg.(wsInflightMessage)
						latency := timeRead.Sub(inflightMsg.timeSent)
						e.inflightMsgs.Delete(responseIDStr)

						// 4. Restore original ID and marshal the JSON-RPC interface back into a byte slice
						jsonRPCResp.id = inflightMsg.originalID
						p, err = jsonRPCResp.MarshalJSON()
						if err != nil {
							// Send an error response
							e.respChan <- errorResponse(err, e.id, e.URL)
							return
						}
						// 5. set metadata to the taskresponse: original id, duration between time sent and time received
						taskResponse := NewWSTaskResponse(p)
						taskResponse.latency = latency
						taskResponse.receivedAt = timeRead

						// Send the message to the read channel
						response := WatcherResponse{
							WatcherID: e.id,
							URL:       e.URL,
							Err:       nil,
							Payload:   taskResponse,
						}
						e.respChan <- response
					}
				}
			} else {
				// Send the message to the read channel
				response := WatcherResponse{
					WatcherID: e.id,
					URL:       e.URL,
					Err:       nil,
					Payload:   NewWSTaskResponse(p),
				}
				e.respChan <- response
			}
		}
	}
}

// wsOneHit is an implementation of taskman.Task that sets up a short-lived WebSocket connection
// to send a message to the endpoint. This is useful for endpoints that require a new connection
// for each message, or for situations where there is no way to link the response to the request.
type wsOneHit struct {
	wsEndpoint *WSEndpoint
}

// Execute sets up a WebSocket connection to the WebSocket endpoint, sends a message, and reads
// the response.
// Note: for concurrency safety, the connection's WriteMessage method is used exclusively here.
func (oh *wsOneHit) Execute() error {
	// The connection should not be open
	if oh.wsEndpoint.conn != nil {
		return errors.New("connection is already open")
	}

	oh.wsEndpoint.lock()
	defer oh.wsEndpoint.unlock()

	select {
	case <-oh.wsEndpoint.ctx.Done():
		// Endpoint shutting down, do nothing
		return nil
	default:
		// 1. Establish a new connection
		conn, _, err := websocket.DefaultDialer.Dial(oh.wsEndpoint.URL.String(), oh.wsEndpoint.Header)
		if err != nil {
			err = fmt.Errorf("failed to dial: %w", err)
			oh.wsEndpoint.respChan <- errorResponse(err, oh.wsEndpoint.id, oh.wsEndpoint.URL)
			return err
		}
		defer conn.Close()

		// 2. Write message to connection
		writeStart := time.Now()
		if err := conn.WriteMessage(websocket.TextMessage, oh.wsEndpoint.Payload); err != nil {
			// An error is unexpected, since the connection was just established
			err = fmt.Errorf("failed to write message: %w", err)
			oh.wsEndpoint.respChan <- errorResponse(err, oh.wsEndpoint.id, oh.wsEndpoint.URL)
			return err
		}

		// 3. Read exactly one response
		_, message, err := conn.ReadMessage()
		if err != nil {
			// An error is unexpected, since the connection was just established
			err = fmt.Errorf("failed to read message: %w", err)
			oh.wsEndpoint.respChan <- errorResponse(err, oh.wsEndpoint.id, oh.wsEndpoint.URL)
			return err
		}
		messageRTT := time.Since(writeStart)

		// 4. Create a task response
		taskResponse := NewWSTaskResponse(message)
		taskResponse.latency = messageRTT
		taskResponse.receivedAt = time.Now()

		// 5. Send the response message on the channel
		oh.wsEndpoint.respChan <- WatcherResponse{
			WatcherID: oh.wsEndpoint.id,
			URL:       oh.wsEndpoint.URL,
			Err:       nil,
			Payload:   taskResponse,
		}

		// 6. Close the connection gracefully
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
		err = conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(3*time.Second))
		if err != nil {
			// We tried a graceful close, but maybe the connection is already gone
			return fmt.Errorf("failed to write close message: %w", err)
		}

		// 7. Skip waiting for the server's close message, exit function to close the connection
	}

	return nil
}

// wsPersistent is an implementation of taskman.Task that sends a message on a persistent
// WebSocket connection.
type wsPersistent struct {
	wsEndpoint *WSEndpoint
	protocol   wsPersistentProtocol
}

// wsPersistentProtocol is an enum for the communication protocol used by the long-lived
// WebSocket connection.
type wsPersistentProtocol int

const (
	UnknownProtocol wsPersistentProtocol = iota
	JSONRPC
)

// Execute sends a message to the WebSocket endpoint.
// Note: for concurrency safety, the connection's WriteMessage method is used exclusively here.
func (ll *wsPersistent) Execute() error {
	if ll.protocol != JSONRPC {
		return errors.New("unsupported protocol")
	}

	// If the connection is closed, try to reconnect
	if ll.wsEndpoint.conn == nil || ll.wsEndpoint.conn.UnderlyingConn() == nil {
		if err := ll.wsEndpoint.reconnect(); err != nil {
			return err
		}
	}

	ll.wsEndpoint.lock()
	defer ll.wsEndpoint.unlock()

	select {
	case <-ll.wsEndpoint.ctx.Done():
		// Endpoint shutting down, do nothing
		return nil
	default:
		// Prepare shadowed variables for the message
		var payload []byte
		var err error

		// 1. Unmarshal the msg into a JSON-RPC interface
		jsonRPCReq := &JSONRPCRequest{}
		if len(ll.wsEndpoint.Payload) > 0 {
			// TODO: optimize this to only get the ID?
			err := jsonRPCReq.UnmarshalJSON(ll.wsEndpoint.Payload)
			if err != nil {
				ll.wsEndpoint.respChan <- errorResponse(err, ll.wsEndpoint.id, ll.wsEndpoint.URL)
				return fmt.Errorf("failed to unmarshal JSON-RPC message: %w", err)
			}
		}

		// 2. Generate a random ID and extract the original ID from the JSON-RPC interface
		inflightID := xid.New().String()
		var originalID interface{}
		if !jsonRPCReq.IsEmpty() {
			originalID = jsonRPCReq.ID
			jsonRPCReq.ID = inflightID
		}

		// 3. store the id in a "inflight map" in WSEndpoint, with metadata: original id, time sent
		inflightMsg := wsInflightMessage{
			inflightID: inflightID,
			originalID: originalID,
		}

		// 4. Marshal the updated JSON-RPC interface back into text message
		payload, err = sonic.Marshal(jsonRPCReq)
		if err != nil {
			ll.wsEndpoint.respChan <- errorResponse(err, ll.wsEndpoint.id, ll.wsEndpoint.URL)
			return fmt.Errorf("failed to marshal JSON-RPC message: %w", err)
		}
		inflightMsg.timeSent = time.Now()

		// 5. Store the inflight message in the WSEndpoint
		ll.wsEndpoint.inflightMsgs.Store(inflightID, inflightMsg)

		// If the payload is nil, use the endpoint's payload
		if payload == nil {
			payload = ll.wsEndpoint.Payload
		}

		// Write message to connection
		if err := ll.wsEndpoint.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				// This is an expected situation, handle gracefully
			} else if strings.Contains(err.Error(), "websocket: close sent") {
				// This is an expected situation, handle gracefully
			} else {
				// This is unexpected
				// TODO: add custom handling here?
			}

			// If there was an error, close the connection
			ll.wsEndpoint.Close()

			// Send an error response
			ll.wsEndpoint.respChan <- errorResponse(err, ll.wsEndpoint.id, ll.wsEndpoint.URL)
			return fmt.Errorf("failed to write message: %w", err)
		}
	}

	return nil
}

// errorResponse is a helper to create a WatcherResponse with an error.
func errorResponse(err error, id xid.ID, url *url.URL) WatcherResponse {
	return WatcherResponse{
		WatcherID: id,
		URL:       url,
		Err:       err,
		Payload:   nil,
	}
}
