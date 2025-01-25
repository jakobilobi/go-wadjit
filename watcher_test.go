package wadjit

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/rs/xid"
	"github.com/stretchr/testify/assert"
)

func TestWatcherInitialization(t *testing.T) {
	id := xid.New()
	cadence := 1 * time.Second
	payload := []byte("test payload")
	httpTasks := []HTTPEndpoint{{URL: &url.URL{Scheme: "http", Host: "localhost:8080"}}}
	var tasks []WatcherTask
	for _, task := range httpTasks {
		tasks = append(tasks, &task)
	}
	watcher, err := NewWatcher(id, cadence, payload, tasks)
	assert.NoError(t, err)

	assert.Equal(t, id, watcher.ID())
	assert.Equal(t, cadence, watcher.cadence)
	assert.Equal(t, payload, watcher.payload)
	assert.NotNil(t, watcher.doneChan)
	assert.NotNil(t, watcher.taskResponses)
	assert.NotNil(t, watcher.watcherTasks)
}

func TestWatcherStart(t *testing.T) {
	id := xid.New()
	cadence := 1 * time.Second
	payload := []byte("test payload")
	responseChan := make(chan WatcherResponse)

	watcher := &Watcher{
		id:      id,
		cadence: cadence,
		payload: payload,
		watcherTasks: []WatcherTask{
			&HTTPEndpoint{
				URL:    &url.URL{Scheme: "http", Host: "localhost:8080"},
				Header: make(http.Header),
			},
		},
	}

	err := watcher.Start(responseChan)
	assert.NoError(t, err)
	assert.NotNil(t, watcher.watcherTasks)

	// These are not nil, even though uninitialized, since Start initializes them if found nil
	assert.NotNil(t, watcher.doneChan)
	assert.NotNil(t, watcher.taskResponses)
}

func TestWatcherExecution(t *testing.T) {
	server := echoServer()
	defer server.Close()

	// Set up URLs
	httpURL, err := url.Parse(server.URL)
	assert.NoError(t, err, "failed to parse HTTP URL")
	wsURL, err := url.Parse("ws" + server.URL[4:] + "/ws")
	assert.NoError(t, err, "failed to parse WS URL")
	header := make(http.Header)

	// Set up watcher
	id := xid.New()
	cadence := 1 * time.Second
	payload := []byte("test payload")
	var tasks []WatcherTask
	tasks = append(tasks, &HTTPEndpoint{URL: httpURL, Header: header})
	tasks = append(tasks, &WSConnection{URL: wsURL, Header: header})
	watcher, err := NewWatcher(id, cadence, payload, tasks)
	assert.NoError(t, err)

	// Start the watcher and execute the tasks
	watcherResponses := make(chan WatcherResponse)
	err = watcher.Start(watcherResponses)
	assert.NoError(t, err)
	for _, task := range watcher.watcherTasks {
		task.Task(payload).Execute()
	}

	// Listen for responses on the watcherResponses channel
	for i := 0; i < len(watcher.watcherTasks); i++ {
		response := <-watcherResponses
		assert.NotNil(t, response)
		assert.NotNil(t, response.URL)
		assert.Nil(t, response.Err)
		assert.Equal(t, id, response.WatcherID)
		assert.NotNil(t, response.Payload)
		responsePayload, err := response.Payload.Data()
		assert.NoError(t, err)
		assert.Equal(t, payload, responsePayload)
		if response.URL.Scheme == "http" {
			_, ok := response.Payload.(*HTTPTaskResponse)
			assert.True(t, ok, "response.Payload is not of type HTTPTaskResponse")
		} else if response.URL.Scheme == "ws" {
			_, ok := response.Payload.(*WSTaskResponse)
			assert.True(t, ok, "response.Payload is not of type WSTaskResponse")
		} else {
			t.Fail()
		}
	}
}
