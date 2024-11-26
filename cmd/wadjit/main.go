package main

import (
	"time"

	"github.com/jakobilobi/wadjit/pkg/scheduler"
	"github.com/rs/zerolog/log"
)

// ExampleTask is an example task implementation, used in tests and development.
type ExampleTask struct {
	// TODO: consider creating a unique TaskID type
	ID string // Unique identifier for the task
	// TODO: also consider a GroupID for grouping tasks
	cadence time.Duration
}

// Cadence returns the cadence of the ExampleTask.
func (dt ExampleTask) Cadence() time.Duration {
	return dt.cadence
}

// Execute executes the ExampleTask.
func (dt ExampleTask) Execute() scheduler.Result {
	log.Trace().Msgf("Executing task %s", dt.ID)
	// Placeholder: Implement task execution logic
	return scheduler.Result{}
}

// NewExampleTask creates and returns a new ExampleTask.
func NewExampleTask(id string, cadence time.Duration) *ExampleTask {
	return &ExampleTask{
		ID:      id,
		cadence: cadence,
	}
}

func main() {
	// TODO: set from scheduler function, e.g. 'for result := range scheduler.Results() {' from within a goroutine
	resultChannel := make(chan scheduler.Result, 100) // Buffered channel to hold results

	// TODO: add flags for configuring the worker pool size and refresh rate etc.

	// TODO: evaluate and adjust buffer sizes
	taskScheduler := scheduler.NewScheduler(10, 8, 8)

	// TODO: add endpoint DB client and fetch tasks from the DB, add these as tasks in scheduler
	// DEV: add tasks with varying cadences
	taskScheduler.AddTask(NewExampleTask("http://example.com/2", 2*time.Second), "")
	taskScheduler.AddTask(NewExampleTask("http://example.com/3", 3*time.Second), "")
	taskScheduler.AddTask(NewExampleTask("http://example.com/5", 5*time.Second), "")

	// Process results and write to the external database
	// TODO: implement actual processing logic
	for result := range resultChannel {
		if result.Error != nil {
			log.Printf("Task failed: %v", result.Error)
		} else {
			log.Printf("Task succeeded")
		}
	}
}
