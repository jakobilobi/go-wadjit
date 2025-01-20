package wadjit

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/go-multierror"
	"github.com/jakobilobi/go-taskman"
	"github.com/rs/xid"
)

// TODO: consider if it's feasible to implement subscriptions, e.g. as another "task type" or even another watcher type

// Watcher is a watcher that sends HTTP requests and WS messages to endpoints, and then
// forwards the responses to a response channel.
type Watcher struct {
	id      xid.ID
	cadence time.Duration
	payload []byte

	watcherTasks []WatcherTask

	taskResponses chan WatcherResponse
	doneChan      chan struct{}
}

// WatcherResponse represents a response from a watcher.
type WatcherResponse struct {
	WatcherID    xid.ID
	URL          *url.URL
	Err          error
	WSData       []byte         // For WS
	HTTPResponse *http.Response // For HTTP
}

// Data returns the data from the response.
func (r WatcherResponse) Data() ([]byte, error) {
	// Check for errors
	if r.Err != nil {
		return nil, r.Err
	}
	// Check for duplicate data, which is undefined behavior
	if r.WSData != nil && r.HTTPResponse != nil {
		return nil, errors.New("both WS and HTTP data available")
	}
	// Check for WS data
	if r.WSData != nil {
		return r.WSData, nil
	}
	// Check for HTTP data
	if r.HTTPResponse != nil {
		// Read the body
		defer r.HTTPResponse.Body.Close()
		buf := new(bytes.Buffer)
		buf.ReadFrom(r.HTTPResponse.Body)
		return buf.Bytes(), nil
	}
	return nil, errors.New("no data available")
}

// errorResponse is a helper to create a WatcherResponse with an error.
func errorResponse(err error, url *url.URL) WatcherResponse {
	return WatcherResponse{
		WatcherID:    xid.NilID(),
		URL:          url,
		Err:          err,
		WSData:       nil,
		HTTPResponse: nil,
	}
}

// WatcherTask is a task that the Watcher can execute to interact with a target endpoint.
type WatcherTask interface {
	// Close closes the WatcherTask, cleaning up and releasing resources.
	// Note: will block until the task is closed. (?)
	Close() error

	// Initialize sets up the WatcherTask to be ready to watch an endpoint.
	Initialize(respChan chan WatcherResponse) error

	// Task returns a taskman.Task that sends requests and messages to the endpoint.
	Task(payload []byte) taskman.Task
}

// Close closes the HTTP watcher.
func (w *Watcher) Close() error {
	// Signal that the watcher is done
	close(w.doneChan)

	// Close all WS connections
	var result *multierror.Error
	for i := range w.watcherTasks {
		err := w.watcherTasks[i].Close()
		if err != nil {
			result = multierror.Append(result, err)
		}
	}
	return result.ErrorOrNil()
}

// ID returns the ID of the Watcher.
func (w *Watcher) ID() xid.ID {
	return w.id
}

// Job returns a taskman.Job that executes the Watcher's tasks.
func (w *Watcher) Job() taskman.Job {
	tasks := make([]taskman.Task, 0, len(w.watcherTasks))
	for i := range w.watcherTasks {
		tasks = append(tasks, w.watcherTasks[i].Task(w.payload))
	}
	// Create the job
	job := taskman.Job{
		ID:       w.id.String(),
		Cadence:  w.cadence,
		NextExec: time.Now().Add(w.cadence),
		Tasks:    tasks,
	}
	return job
}

// Initialize sets up the Watcher to start listening for responses, and initializes its tasks.
func (w *Watcher) Initialize(responseChan chan WatcherResponse) error {
	var result *multierror.Error
	// If the response channel is nil, the watcher cannot function
	if responseChan == nil {
		result = multierror.Append(result, errors.New("response channel is nil"))
	}

	// Initialize the internal channels
	w.doneChan = make(chan struct{})
	w.taskResponses = make(chan WatcherResponse)

	// Initialize the watcher tasks
	for i := range w.watcherTasks {
		err := w.watcherTasks[i].Initialize(w.taskResponses)
		if err != nil {
			result = multierror.Append(result, err)
		}
	}

	// Start the goroutine that forwards responses to the response channel
	go w.forwardResponses(responseChan)

	return result.ErrorOrNil()
}

// forwardResponses listens for responses from the Watcher's requests, and forwards them to
// the responseChan.
func (w *Watcher) forwardResponses(responseChan chan WatcherResponse) {
	for {
		select {
		case resp := <-w.taskResponses:
			// Attach Watcher ID to the response
			// TODO: is there some better way to attach the ID than intercepting the response?
			resp.WatcherID = w.id
			responseChan <- resp
		case <-w.doneChan:
			return
		}
	}
}

// HTTPEndpoint represents an HTTP endpoint that the Watcher can interact with.
type HTTPEndpoint struct {
	URL    *url.URL
	Header http.Header

	respChan chan<- WatcherResponse
}

// Close closes the HTTP endpoint.
func (e *HTTPEndpoint) Close() error {
	return nil
}

// Initialize sets up the HTTP endpoint to be able to send on its responses.
func (e *HTTPEndpoint) Initialize(responseChannel chan WatcherResponse) error {
	e.respChan = responseChannel
	return nil
}

// Task returns a taskman.Task that sends an HTTP request to the endpoint.
func (e *HTTPEndpoint) Task(payload []byte) taskman.Task {
	return &httpRequest{
		Header:   e.Header,
		Method:   http.MethodGet,
		URL:      e.URL,
		Data:     payload,
		respChan: e.respChan,
	}
}

// WSConnection represents and handles a WebSocket connection.
// TODO: add a reconnect mechanism?
type WSConnection struct {
	conn *websocket.Conn
	mu   sync.Mutex

	URL    *url.URL
	Header http.Header

	writeChan chan []byte
	respChan  chan<- WatcherResponse

	ctx    context.Context
	cancel context.CancelFunc
}

// Close closes the WebSocket connection, and cancels its context.
func (c *WSConnection) Close() error {
	c.cancel()
	return c.conn.Close()
}

// Initialize sets up the WebSocket connection.
func (c *WSConnection) Initialize(responseChannel chan WatcherResponse) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	newConn, _, err := websocket.DefaultDialer.Dial(c.URL.Host, c.Header)
	if err != nil {
		return err
	}
	c.conn = newConn
	c.writeChan = make(chan []byte)
	c.respChan = responseChannel
	c.ctx, c.cancel = context.WithCancel(context.Background())

	go c.read()

	return nil
}

// Task returns a taskman.Task that sends a message to the WebSocket endpoint.
func (c *WSConnection) Task(payload []byte) taskman.Task {
	return &wsSend{
		conn: c,
		msg:  payload,
	}
}

// lock and unlock provide exclusive access to the connection's mutex.
func (c *WSConnection) lock() {
	c.mu.Lock()
}

func (c *WSConnection) unlock() {
	c.mu.Unlock()
}

// read reads messages from the WebSocket connection.
// Note: the read pump has exclusive permission to read from the connection.
func (c *WSConnection) read() {
	defer c.cancel()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			// TODO: reset read deadlines before reading ???

			// Read message from connection
			_, p, err := c.conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseServiceRestart) {
					// This is an expected situation, handle gracefully
				} else if strings.Contains(err.Error(), "connection closed") {
					// This is not an unknown situation, handle gracefully
				} else {
					// This is unexpected
				}

				// If there was an error, close the connection
				return
			}

			// Send the message to the read channel
			response := WatcherResponse{
				WatcherID:    xid.NilID(),
				URL:          c.URL,
				Err:          nil,
				WSData:       p,
				HTTPResponse: nil,
			}
			c.respChan <- response
		}
	}
}

// httpRequest is an implementation of taskman.Task that sends an HTTP request to an endpoint.
type httpRequest struct {
	Header http.Header
	Method string
	URL    *url.URL
	Data   []byte

	respChan chan<- WatcherResponse
}

// Execute sends an HTTP request to the endpoint.
func (r httpRequest) Execute() error {
	request, err := http.NewRequest(r.Method, r.URL.String(), bytes.NewReader(r.Data))
	if err != nil {
		r.respChan <- errorResponse(err, r.URL)
		return err
	}

	for key, values := range r.Header {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		r.respChan <- errorResponse(err, r.URL)
		return err
	}

	// Send the response without reading it, leaving that to the Watcher's owner
	r.respChan <- WatcherResponse{
		WatcherID:    xid.NilID(),
		URL:          r.URL,
		Err:          nil,
		WSData:       nil,
		HTTPResponse: response,
	}

	return nil
}

// wsSend is an implementation to taskman.Task that sends a message to a WebSocket endpoint.
type wsSend struct {
	conn *WSConnection
	msg  []byte
}

// Execute sends a message to the WebSocket endpoint.
// Note: for concurrency safety, the connection's WriteMessage method is used exclusively here.
func (ws *wsSend) Execute() error {
	ws.conn.lock()
	defer ws.conn.unlock()

	select {
	case <-ws.conn.ctx.Done():
		// The connection has been closed
		return nil
	default:
		// TODO: use/set a write deadline ???
		if err := ws.conn.conn.WriteMessage(websocket.TextMessage, ws.msg); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				// This is an expected situation, handle gracefully
			} else if strings.Contains(err.Error(), "websocket: close sent") {
				// This is an expected situation, handle gracefully
			} else {
				// This is unexpected
			}

			// TODO: if there was an error, close the connection? reconnect?

			ws.conn.respChan <- errorResponse(err, ws.conn.URL)

			return err
		}
	}

	return nil
}
