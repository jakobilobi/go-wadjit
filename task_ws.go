package wadjit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"github.com/jkbrsn/go-jsonrpc"
	"github.com/jkbrsn/go-taskman"
	"github.com/rs/xid"
)

// WSEndpoint connects to the target endpoint, and spawns tasks to send messages to that endpoint.
// Implements the WatcherTask interface and is meant for use in a Watcher.
type WSEndpoint struct {
	mu sync.Mutex

	Header  http.Header
	Mode    WSEndpointMode
	Payload []byte
	URL     *url.URL
	ID      string

	// Set internally
	conn         *websocket.Conn
	remoteAddr   net.Addr
	inflightMsgs sync.Map // Key string to value wsInflightMessage
	wg           sync.WaitGroup

	// Set by Initialize
	watcherID string
	respChan  chan<- WatcherResponse
	ctx       context.Context
	cancel    context.CancelFunc
}

// WSEndpointMode is an enum for the mode of the WebSocket endpoint.
type WSEndpointMode int

const (
	ModeUnknown WSEndpointMode = iota // Defaults to OneHitText
	// One hit text mode is the basic mode where a new connection is established for each message
	// and the response is read once. This design is due to the nature of standard text-based
	// WebSocket messages not having a way to link responses to requests.
	OneHitText
	// Persistent JSON RPC mode is a mode where a long-lived connection is established to the
	// endpoint, and JSON-RPC messages are sent and received. This mode sets a temporary ID for
	// each message, which is used to link an incoming response to the request. This allows for
	// reuse of the same connection for multiple messages while keeping message integrity.
	PersistentJSONRPC
)

// wsInflightMessage stores metadata about a message that is currently in-flight.
type wsInflightMessage struct {
	inflightID string
	originalID any
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
func (e *WSEndpoint) Initialize(watcherID string, responseChannel chan<- WatcherResponse) error {
	e.mu.Lock()
	e.watcherID = watcherID
	e.respChan = responseChannel
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.mu.Unlock()

	switch e.Mode {
	case PersistentJSONRPC:
		err := e.connect()
		if err != nil {
			return fmt.Errorf("failed to connect when initializing: %w", err)
		}
	case OneHitText:
		// One hit modes do not require a connection to be established, so do nothing
	default:
		// Default to one hit text mode, since its a mode not requiring anything logic outside of its own scope
		e.mu.Lock()
		e.Mode = OneHitText
		e.mu.Unlock()
	}

	return nil
}

// Task returns a taskman.Task that sends a message to the WebSocket endpoint.
func (e *WSEndpoint) Task() taskman.Task {
	switch e.Mode {
	case OneHitText:
		return &wsOneHit{
			wsEndpoint: e,
		}
	case PersistentJSONRPC:
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
	if e.ID == "" {
		// Set random ID if nil
		e.ID = xid.New().String()
	}
	return nil
}

// closeConn closes the WebSocket connection without closing the context.
func (e *WSEndpoint) closeConn() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.conn != nil {
		err := e.conn.Close()
		e.conn = nil
		return err
	}

	return nil
}

// connect establishes a connection to the WebSocket endpoint. If already connected,
// this function does nothing.
func (e *WSEndpoint) connect() error {
	if e.Mode != PersistentJSONRPC {
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
	e.remoteAddr = conn.RemoteAddr()

	// Start the read pump for incoming messages
	e.wg.Add(1)
	go e.readPump(&e.wg)

	return nil
}

// nilConn checks if the WebSocket connection is nil or closed.
func (e *WSEndpoint) nilConn() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.conn == nil || e.conn.NetConn() == nil {
		return true
	}
	return false
}

// reconnect closes the current connection and establishes a new one.
func (e *WSEndpoint) reconnect() error {
	if e.Mode != PersistentJSONRPC {
		return errors.New("can only reconnect for long-lived connections")
	}

	// Close the current connection, if it exists
	if !e.nilConn() {
		if err := e.conn.Close(); err != nil {
			return fmt.Errorf("failed to close connection: %w", err)
		}
	}

	// Wait for the read pump to finish
	e.wg.Wait()

	e.mu.Lock()
	defer e.mu.Unlock()

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

			// Clone the URL to avoid downstream mutation
			urlClone := *e.URL

			// Read message from connection
			_, p, err := e.conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseServiceRestart) {
					// This is an expected situation, handle gracefully
				} else if strings.Contains(err.Error(), "connection closed") {
					// This is not an unknown situation, handle gracefully
				} else {
					// This is unexpected
					// TODO: consider returning an error response, though the channel may not be available
					//e.respChan <- errorResponse(fmt.Errorf("unexpected websocket read error: %w", err), e.ID, e.watcherID, &urlClone)
				}

				// If there was an error, close the connection
				e.closeConn()

				return
			}
			// Register first byte timestamp
			timestamps := requestTimestamps{
				firstByte: time.Now(),
			}

			if e.Mode == PersistentJSONRPC {
				// 1. Unmarshal p into a JSON-RPC response interface
				jsonRPCResp, err := jsonrpc.DecodeResponse(p)
				if err != nil {
					// Send an error response
					e.respChan <- errorResponse(fmt.Errorf("failed parsing jsonrpc.Response from bytes: %w", err), e.ID, e.watcherID, &urlClone)
					return
				}

				// 2. Check the ID against the inflight messages map
				if !jsonRPCResp.IsEmpty() {
					responseID := jsonRPCResp.IDString()
					if responseID == "" {
						// Send an error response
						e.respChan <- errorResponse(fmt.Errorf("found nil response ID, error: %s", jsonRPCResp.Result), e.ID, e.watcherID, &urlClone)
						return
					}

					// 3. If the ID is known, get the inflight map metadata and delete the ID in the map
					if inflightMsg, ok := e.inflightMsgs.Load(responseID); ok {
						inflightMsg := inflightMsg.(wsInflightMessage)
						e.inflightMsgs.Delete(responseID)

						// Get start time from inflight message
						timestamps.start = inflightMsg.timeSent

						// 4. Restore original ID and marshal the JSON-RPC interface back into a byte slice
						jsonRPCResp.ID = inflightMsg.originalID
						p, err = jsonRPCResp.MarshalJSON()
						if err != nil {
							// Send an error response
							e.respChan <- errorResponse(fmt.Errorf("failed re-marshalling JSON-RPC response: %w", err), e.ID, e.watcherID, &urlClone)
							return
						}
						// 5. set metadata to the taskresponse: original id, duration between time sent and time received
						taskResponse := NewWSTaskResponse(e.remoteAddr, p)
						taskResponse.timestamps = timestamps

						// Send the message to the read channel
						response := WatcherResponse{
							TaskID:    e.ID,
							WatcherID: e.watcherID,
							URL:       &urlClone,
							Err:       nil,
							Payload:   taskResponse,
						}
						e.respChan <- response
					} else {
						e.respChan <- errorResponse(errors.New("unknown response ID: "+jsonRPCResp.IDString()), e.ID, e.watcherID, &urlClone)
					}
				} else {
					e.respChan <- errorResponse(errors.New("empty JSON-RPC response"), e.ID, e.watcherID, &urlClone)
				}
			} else {
				// Send the message to the read channel
				response := WatcherResponse{
					TaskID:    e.ID,
					WatcherID: e.watcherID,
					URL:       &urlClone,
					Err:       nil,
					Payload:   NewWSTaskResponse(e.remoteAddr, p),
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

	// Clone the URL to avoid downstream mutation
	urlClone := *oh.wsEndpoint.URL

	select {
	case <-oh.wsEndpoint.ctx.Done():
		// Endpoint shutting down, do nothing
		return nil
	default:
		timestamps := requestTimestamps{}

		// 1. Establish a new connection
		timestamps.start = time.Now()
		// TODO: move DNS, conn and TLS timings into the dialer, perhaps reuse wsstat library?
		timestamps.dnsStart = time.Now()
		timestamps.connStart = time.Now()
		timestamps.tlsStart = time.Now()
		conn, _, err := websocket.DefaultDialer.Dial(urlClone.String(), oh.wsEndpoint.Header)
		if err != nil {
			err = fmt.Errorf("failed to dial: %w", err)
			oh.wsEndpoint.respChan <- errorResponse(err, oh.wsEndpoint.ID, oh.wsEndpoint.watcherID, &urlClone)
			return err
		}
		remoteAddr := conn.RemoteAddr()
		defer conn.Close()
		timestamps.dnsDone = time.Now()
		timestamps.connDone = time.Now()
		timestamps.tlsDone = time.Now()

		// 2. Write message to connection
		if err := conn.WriteMessage(websocket.TextMessage, oh.wsEndpoint.Payload); err != nil {
			// An error is unexpected, since the connection was just established
			err = fmt.Errorf("failed to write message: %w", err)
			oh.wsEndpoint.respChan <- errorResponse(err, oh.wsEndpoint.ID, oh.wsEndpoint.watcherID, &urlClone)
			return err
		}
		timestamps.wroteDone = time.Now()

		// 3. Read exactly one response
		_, message, err := conn.ReadMessage()
		if err != nil {
			// An error is unexpected, since the connection was just established
			err = fmt.Errorf("failed to read message: %w", err)
			oh.wsEndpoint.respChan <- errorResponse(err, oh.wsEndpoint.ID, oh.wsEndpoint.watcherID, &urlClone)
			return err
		}
		timestamps.firstByte = time.Now() // TODO: can we properly get this at the first byte instead of after read?
		timestamps.dataDone = time.Now()

		// 4. Create a task response
		taskResponse := NewWSTaskResponse(remoteAddr, message)
		taskResponse.timestamps = timestamps

		// 5. Send the response message on the channel
		oh.wsEndpoint.respChan <- WatcherResponse{
			TaskID:    oh.wsEndpoint.ID,
			WatcherID: oh.wsEndpoint.watcherID,
			URL:       &urlClone,
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
	if ll.wsEndpoint.nilConn() {
		if err := ll.wsEndpoint.reconnect(); err != nil {
			return err
		}
	}

	ll.wsEndpoint.lock()
	defer ll.wsEndpoint.unlock()

	// Clone the URL to avoid downstream mutation
	urlClone := *ll.wsEndpoint.URL

	select {
	case <-ll.wsEndpoint.ctx.Done():
		// Endpoint shutting down, do nothing
		return nil
	default:
		// Prepare shadowed variables for the message
		var payload []byte
		var err error

		// 1. Unmarshal the msg into a JSON-RPC interface
		jsonRPCReq := &jsonrpc.Request{}
		if len(ll.wsEndpoint.Payload) > 0 {
			// TODO: optimize this to only get the ID?
			err := jsonRPCReq.UnmarshalJSON(ll.wsEndpoint.Payload)
			if err != nil {
				err = fmt.Errorf("failed to unmarshal JSON-RPC message: %w", err)
				ll.wsEndpoint.respChan <- errorResponse(err, ll.wsEndpoint.ID, ll.wsEndpoint.watcherID, &urlClone)
				return err
			}
		}

		// 2. Generate a random ID and extract the original ID from the JSON-RPC interface
		inflightID := xid.New().String()
		var originalID any
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
			err = fmt.Errorf("failed to marshal JSON-RPC message: %w", err)
			ll.wsEndpoint.respChan <- errorResponse(err, ll.wsEndpoint.ID, ll.wsEndpoint.watcherID, &urlClone)
			return err
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
				err = fmt.Errorf("websocket write failed (connection closed): %w", err)
			} else if strings.Contains(err.Error(), "websocket: close sent") {
				// This is an expected situation, handle gracefully
				err = fmt.Errorf("websocket write failed (connection closed): %w", err)
			} else {
				// This is unexpected
				err = fmt.Errorf("unexpected websocket write error: %w", err)
			}

			// Close the connection
			ll.wsEndpoint.closeConn()

			// Send an error response
			ll.wsEndpoint.respChan <- errorResponse(err, ll.wsEndpoint.ID, ll.wsEndpoint.watcherID, &urlClone)
			return err
		}
	}

	return nil
}

// NewWSEndpoint creates a new WSEndpoint with the given attributes.
func NewWSEndpoint(
	url *url.URL,
	header http.Header,
	mode WSEndpointMode,
	payload []byte,
	id string,
) *WSEndpoint {
	return &WSEndpoint{
		Header:  header,
		Mode:    mode,
		Payload: payload,
		URL:     url,
		ID:      id,
	}
}
