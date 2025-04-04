package wadjit

import (
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/xid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewWadjit(t *testing.T) {
	w := New()
	defer w.Close()

	assert.NotNil(t, w)
	assert.NotNil(t, w.taskManager)
}

func TestAddWatcher(t *testing.T) {
	w := New()
	defer w.Close()

	// Create a watcher
	id := xid.New().String()
	cadence := 1 * time.Second
	payload := []byte("test payload")
	httpTasks := []HTTPEndpoint{{URL: &url.URL{Scheme: "http", Host: "localhost:8080"}, Payload: payload}}
	var tasks []WatcherTask
	for _, task := range httpTasks {
		tasks = append(tasks, &task)
	}
	watcher, err := NewWatcher(id, cadence, tasks)
	assert.NoError(t, err, "error creating watcher")

	// Add the watcher
	err = w.AddWatcher(watcher)
	assert.NoError(t, err, "error adding watcher")
	w.wadjitStarted <- struct{}{}    // Unblock Watcher adding (otherwise done by w.ResponseChannel())
	time.Sleep(5 * time.Millisecond) // wait for watcher to be added

	// Check that the watcher was added correctly
	assert.Equal(t, 1, syncMapLen(&w.watchers))
	loaded, _ := w.watchers.Load(id)
	assert.NotNil(t, loaded)
	loaded = loaded.(*Watcher)
	assert.Equal(t, watcher, loaded)
}

func TestRemoveWatcher(t *testing.T) {
	w := New()
	defer w.Close()

	// Create a watcher
	id := xid.New().String()
	cadence := 1 * time.Second
	payload := []byte("test payload")
	httpTasks := []HTTPEndpoint{{URL: &url.URL{Scheme: "http", Host: "localhost:8080"}, Payload: payload}}
	var tasks []WatcherTask
	for _, task := range httpTasks {
		tasks = append(tasks, &task)
	}
	watcher, err := NewWatcher(id, cadence, tasks)
	assert.NoError(t, err, "error creating watcher")

	err = w.AddWatcher(watcher)
	assert.NoError(t, err, "error adding watcher")
	w.wadjitStarted <- struct{}{}    // Unblock Watcher adding (otherwise done by w.ResponseChannel())
	time.Sleep(5 * time.Millisecond) // Wait for watcher to be added

	assert.Equal(t, 1, syncMapLen(&w.watchers))

	err = w.RemoveWatcher(id)
	assert.NoError(t, err)
	assert.Equal(t, 0, syncMapLen(&w.watchers))

	loaded, ok := w.watchers.Load(id)
	assert.Nil(t, loaded)
	assert.False(t, ok)
}

func TestWadjitLifecycle(t *testing.T) {
	w := New()
	server := echoServer()
	defer func() {
		// Make sure the Wadjit is closed before the server closes
		w.Close()
		server.Close()
	}()

	// Set up URLs
	url, err := url.Parse(server.URL)
	assert.NoError(t, err, "failed to parse HTTP URL")

	// Create first watcher
	id1 := xid.New().String()
	tasks := append([]WatcherTask{}, &HTTPEndpoint{URL: url, Method: http.MethodPost, Payload: []byte("first")})
	watcher1, err := NewWatcher(
		id1,
		5*time.Millisecond,
		tasks,
	)
	assert.NoError(t, err, "error creating watcher 1")
	// Create second watcher
	id2 := xid.New().String()
	tasks = append([]WatcherTask{}, &HTTPEndpoint{URL: url, Method: http.MethodPost, Payload: []byte("second")})
	watcher2, err := NewWatcher(
		id2,
		15*time.Millisecond,
		tasks,
	)
	assert.NoError(t, err, "error creating watcher 2")
	// Create third watcher
	id3 := xid.New().String()
	wsURL := "ws" + server.URL[4:] + "/ws"
	url, err = url.Parse(wsURL)
	assert.NoError(t, err, "failed to parse URL")
	tasks = append([]WatcherTask{}, &WSEndpoint{URL: url, Payload: []byte("third")})
	watcher3, err := NewWatcher(
		id3,
		10*time.Millisecond,
		tasks,
	)
	assert.NoError(t, err, "error creating watcher 3")

	// Consume responses
	responses := w.Start()
	firstCount := atomic.Int32{}
	secondCount := atomic.Int32{}
	thirdCount := atomic.Int32{}
	go func() {
		for {
			select {
			case response := <-responses:
				assert.NoError(t, response.Err)
				require.NotNil(t, response.Payload)
				data, err := response.Payload.Data()
				assert.NoError(t, err)
				if string(data) == "first" {
					firstCount.Add(1)
				} else if string(data) == "second" {
					secondCount.Add(1)
				} else if string(data) == "third" {
					thirdCount.Add(1)
				}
			case <-w.ctx.Done():
				return
			}
		}
	}()

	// Add watchers
	err = w.AddWatchers(watcher1, watcher2, watcher3)
	assert.NoError(t, err, "error adding watchers")
	time.Sleep(5 * time.Millisecond) // Wait for watchers to be added
	assert.Equal(t, 3, syncMapLen(&w.watchers))

	// Let the watchers run
	time.Sleep(25 * time.Millisecond)

	// Confirm the first watcher executed more tasks than the second
	assert.Greater(t, firstCount.Load(), secondCount.Load())
	assert.Greater(t, firstCount.Load(), thirdCount.Load())
	assert.Greater(t, thirdCount.Load(), secondCount.Load())

	// Remove the first watcher
	err = w.RemoveWatcher(id1)
	assert.NoError(t, err)
	assert.Equal(t, 2, syncMapLen(&w.watchers))

	// Try adding the same Watcher again
	err = w.AddWatcher(watcher1)
	assert.Error(t, err, "expected error adding a closed watcher")

	// Try adding a new Watcher, but with the ID of the removed Watcher
	tasks = append([]WatcherTask{}, &HTTPEndpoint{URL: url, Method: http.MethodPost, Payload: []byte("first")})
	watcher4, err := NewWatcher(
		id1,
		5*time.Millisecond,
		tasks,
	)
	assert.NoError(t, err, "error creating watcher 4")
	err = w.AddWatcher(watcher4)
	assert.NoError(t, err)
}
